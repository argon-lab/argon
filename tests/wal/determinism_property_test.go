package wal_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// This file holds the M1 acceptance property: replaying the same WAL prefix
// must always yield the same state — across repeated materializations,
// across independent service instances (separate processes in production),
// and at every historical LSN. The workload below deliberately exercises
// every write path (insert single/batch, update with a mix of operators and
// filters, replace, upsert, delete single/many), plus branch forks and a
// reset, because those are exactly the code paths whose v1 versions were
// nondeterministic.

// applyRandomWorkload drives a seeded pseudo-random operation sequence
// through an interceptor. Everything is derived from the seed, so two runs
// with the same seed issue identical operations.
func applyRandomWorkload(t *testing.T, rng *rand.Rand, interceptor *driverwal.Interceptor, numOps int) {
	t.Helper()
	ctx := context.Background()
	collections := []string{"users", "orders", "items"}

	for i := 0; i < numOps; i++ {
		coll := collections[rng.Intn(len(collections))]
		docID := fmt.Sprintf("doc-%d", rng.Intn(30))

		switch rng.Intn(10) {
		case 0, 1, 2: // insert (may collide with an existing _id; ignore dup errors)
			_, _ = interceptor.InsertOne(ctx, coll, bson.M{
				"_id":   docID,
				"n":     int32(rng.Intn(1000)),
				"group": rng.Intn(5),
				"tags":  []interface{}{fmt.Sprintf("t%d", rng.Intn(3))},
			})
		case 3, 4: // update by _id with typed operators
			_, err := interceptor.UpdateOne(ctx, coll,
				bson.M{"_id": docID},
				bson.M{
					"$inc":  bson.M{"n": int32(rng.Intn(7) - 3)},
					"$set":  bson.M{"touched": int64(i)},
					"$push": bson.M{"tags": fmt.Sprintf("u%d", i%4)},
				}, false)
			require.NoError(t, err)
		case 5: // update many by non-ID filter
			_, err := interceptor.UpdateMany(ctx, coll,
				bson.M{"group": rng.Intn(5)},
				bson.M{"$inc": bson.M{"n": int32(1)}, "$unset": bson.M{"touched": ""}}, false)
			require.NoError(t, err)
		case 6: // upsert
			_, err := interceptor.UpdateOne(ctx, coll,
				bson.M{"_id": fmt.Sprintf("upsert-%d", rng.Intn(10))},
				bson.M{"$set": bson.M{"n": int32(i)}, "$setOnInsert": bson.M{"origin": "upsert"}}, true)
			require.NoError(t, err)
		case 7: // replace
			_, err := interceptor.ReplaceOne(ctx, coll,
				bson.M{"_id": docID},
				bson.M{"replaced": true, "n": int32(i)}, false)
			require.NoError(t, err)
		case 8: // delete one by comparison filter
			_, err := interceptor.DeleteOne(ctx, coll, bson.M{"n": bson.M{"$gt": int32(rng.Intn(900))}})
			require.NoError(t, err)
		case 9: // delete many in a group
			_, err := interceptor.DeleteMany(ctx, coll, bson.M{"group": rng.Intn(5), "n": bson.M{"$lt": int32(rng.Intn(200))}})
			require.NoError(t, err)
		}
	}
}

// requireSameState asserts content-level equality of two branch states.
func requireSameState(t *testing.T, want, got map[string]map[string]bson.M, label string) {
	t.Helper()
	require.Equal(t, len(want), len(got), "%s: collection sets differ", label)
	for coll, wantDocs := range want {
		require.Equal(t, wantDocs, got[coll], "%s: collection %s diverged", label, coll)
	}
}

func TestProperty_ReplayDeterminism(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	tt := timetravel.NewService(walService, mat)
	restoreService := restore.NewService(walService, branchService, mat, tt)

	const seed = 42

	// Build a history that exercises forks and a reset on top of the
	// random single-branch workload.
	main, err := branchService.CreateBranch("prop-test", "main", "")
	require.NoError(t, err)
	mainWriter := driverwal.NewInterceptor(walService, main, branchService, mat)

	rng := rand.New(rand.NewSource(seed))
	applyRandomWorkload(t, rng, mainWriter, 120)

	// Fork a feature branch mid-history and diverge both sides.
	main, err = branchService.GetBranchByID(main.ID)
	require.NoError(t, err)
	feature, err := branchService.CreateBranch("prop-test", "feature", main.ID)
	require.NoError(t, err)
	featureWriter := driverwal.NewInterceptor(walService, feature, branchService, mat)
	applyRandomWorkload(t, rng, featureWriter, 60)
	applyRandomWorkload(t, rng, mainWriter, 60)

	// Reset main part-way back, then write more on top: materialization
	// must skip the discarded window while the pre-reset fork keeps it.
	main, err = branchService.GetBranchByID(main.ID)
	require.NoError(t, err)
	resetTarget := main.HeadLSN - 20
	_, err = restoreService.ResetBranchToLSN(main.ID, resetTarget)
	require.NoError(t, err)
	main, err = branchService.GetBranchByID(main.ID)
	require.NoError(t, err)
	applyRandomWorkload(t, rng, mainWriter, 30)

	main, err = branchService.GetBranchByID(main.ID)
	require.NoError(t, err)
	feature, err = branchService.GetBranchByID(feature.ID)
	require.NoError(t, err)

	t.Run("Repeated materialization is identical", func(t *testing.T) {
		refMain, err := mat.MaterializeBranch(main)
		require.NoError(t, err)
		refFeature, err := mat.MaterializeBranch(feature)
		require.NoError(t, err)
		require.NotEmpty(t, refMain, "workload must have produced state")

		for i := 0; i < 50; i++ {
			gotMain, err := mat.MaterializeBranch(main)
			require.NoError(t, err)
			requireSameState(t, refMain, gotMain, fmt.Sprintf("main run %d", i))

			gotFeature, err := mat.MaterializeBranch(feature)
			require.NoError(t, err)
			requireSameState(t, refFeature, gotFeature, fmt.Sprintf("feature run %d", i))
		}
	})

	t.Run("Independent service instances agree", func(t *testing.T) {
		// A second wal.Service + materializer over the same database is the
		// closest in-process stand-in for a separate reader process.
		walService2, err := wal.NewService(db)
		require.NoError(t, err)
		branchService2, err := branchwal.NewBranchService(db, walService2)
		require.NoError(t, err)
		mat2 := materializer.NewService(walService2, branchService2)

		ref, err := mat.MaterializeBranch(main)
		require.NoError(t, err)
		got, err := mat2.MaterializeBranch(main)
		require.NoError(t, err)
		requireSameState(t, ref, got, "cross-instance")
	})

	t.Run("Historical states are stable", func(t *testing.T) {
		// Pick a few LSNs across the branch's history; each must
		// materialize identically on repeated evaluation.
		lsns := []int64{main.HeadLSN / 4, main.HeadLSN / 2, resetTarget, main.HeadLSN}
		for _, lsn := range lsns {
			if lsn <= 0 {
				continue
			}
			ref, err := tt.GetBranchStateAtLSN(main, lsn)
			require.NoError(t, err)
			for i := 0; i < 10; i++ {
				got, err := tt.GetBranchStateAtLSN(main, lsn)
				require.NoError(t, err)
				requireSameState(t, ref, got, fmt.Sprintf("lsn %d run %d", lsn, i))
			}
		}
	})

	t.Run("Same seed on a fresh database converges to the same state", func(t *testing.T) {
		// Replay the exact same seeded workload (single-branch portion)
		// against a second database: write-time resolution plus
		// deterministic replay must land both databases on identical
		// materialized content.
		db2 := setupTestDB(t)
		walServiceB, err := wal.NewService(db2)
		require.NoError(t, err)
		branchServiceB, err := branchwal.NewBranchService(db2, walServiceB)
		require.NoError(t, err)
		matB := materializer.NewService(walServiceB, branchServiceB)

		mainA, err := branchService.CreateBranch("prop-replay", "main", "")
		require.NoError(t, err)
		mainB, err := branchServiceB.CreateBranch("prop-replay", "main", "")
		require.NoError(t, err)

		writerA := driverwal.NewInterceptor(walService, mainA, branchService, mat)
		writerB := driverwal.NewInterceptor(walServiceB, mainB, branchServiceB, matB)

		rngA := rand.New(rand.NewSource(7))
		rngB := rand.New(rand.NewSource(7))
		applyRandomWorkload(t, rngA, writerA, 100)
		applyRandomWorkload(t, rngB, writerB, 100)

		mainA, _ = branchService.GetBranchByID(mainA.ID)
		mainB, _ = branchServiceB.GetBranchByID(mainB.ID)

		stateA, err := mat.MaterializeBranch(mainA)
		require.NoError(t, err)
		stateB, err := matB.MaterializeBranch(mainB)
		require.NoError(t, err)
		requireSameState(t, stateA, stateB, "cross-database same-seed")
	})
}
