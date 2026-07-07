package wal_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// snapshotFixture wires the full service stack twice over the same
// database: one materializer with snapshots enabled and one without, so
// tests can assert that the accelerated path produces exactly the state a
// full replay produces.
type snapshotFixture struct {
	wal        *wal.Service
	branches   *branchwal.BranchService
	mat        *materializer.Service // snapshot-accelerated
	matFull    *materializer.Service // full replay, no snapshots
	snapshots  *snapshot.Service
	timeTravel *timetravel.Service
	restore    *restore.Service
}

func newSnapshotFixture(t *testing.T, db *mongo.Database) *snapshotFixture {
	t.Helper()
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	mat := materializer.NewService(walService, branchService)
	snapService, err := snapshot.NewService(db, branchService, mat)
	require.NoError(t, err)

	matFull := materializer.NewService(walService, branchService)
	tt := timetravel.NewService(walService, mat)

	return &snapshotFixture{
		wal:        walService,
		branches:   branchService,
		mat:        mat,
		matFull:    matFull,
		snapshots:  snapService,
		timeTravel: tt,
		restore:    restore.NewService(walService, branchService, mat, tt),
	}
}

func (f *snapshotFixture) requireSnapshotMatchesFullReplay(t *testing.T, branch *wal.Branch, label string) {
	t.Helper()
	accelerated, err := f.mat.MaterializeBranch(branch)
	require.NoError(t, err, "%s: accelerated path", label)
	full, err := f.matFull.MaterializeBranch(branch)
	require.NoError(t, err, "%s: full replay path", label)
	requireSameState(t, full, accelerated, label)
}

func TestSnapshot_MatchesFullReplay(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("snap-test", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	rng := rand.New(rand.NewSource(99))
	applyRandomWorkload(t, rng, writer, 100)
	main, _ = f.branches.GetBranchByID(main.ID)

	t.Run("Snapshot at head, then diverge", func(t *testing.T) {
		snaps, err := f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
		require.NoError(t, err)
		require.NotEmpty(t, snaps)

		// State right at the snapshot point.
		f.requireSnapshotMatchesFullReplay(t, main, "at snapshot LSN")

		// More writes on top: snapshot + delta.
		applyRandomWorkload(t, rng, writer, 60)
		main, _ = f.branches.GetBranchByID(main.ID)
		f.requireSnapshotMatchesFullReplay(t, main, "snapshot plus delta")
	})

	t.Run("Historical reads below the snapshot still work", func(t *testing.T) {
		target := main.HeadLSN / 3
		accelerated, err := f.mat.MaterializeCollectionAtLSN(main, "users", target)
		require.NoError(t, err)
		full, err := f.matFull.MaterializeCollectionAtLSN(main, "users", target)
		require.NoError(t, err)
		assert.Equal(t, full, accelerated, "reads below the snapshot fall back to replay")
	})

	t.Run("Child branch uses parent snapshot through ancestry", func(t *testing.T) {
		main, _ = f.branches.GetBranchByID(main.ID)
		child, err := f.branches.CreateBranch("snap-test", "child", main.ID)
		require.NoError(t, err)
		childWriter := walwriter.New(f.wal, f.branches, f.mat, child)
		applyRandomWorkload(t, rng, childWriter, 40)
		child, _ = f.branches.GetBranchByID(child.ID)

		f.requireSnapshotMatchesFullReplay(t, child, "child over parent snapshot")
	})

	t.Run("Snapshot on the child accelerates the whole chain", func(t *testing.T) {
		child, err := f.branches.GetBranch("snap-test", "child")
		require.NoError(t, err)
		_, err = f.snapshots.CreateSnapshot(ctx, child.ID, child.HeadLSN)
		require.NoError(t, err)

		childWriter := walwriter.New(f.wal, f.branches, f.mat, child)
		applyRandomWorkload(t, rng, childWriter, 30)
		child, _ = f.branches.GetBranchByID(child.ID)

		f.requireSnapshotMatchesFullReplay(t, child, "child snapshot plus delta")
	})
}

func TestSnapshot_ResetInvalidation(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("snap-reset", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	// Build history and remember a mid point.
	for i := 0; i < 20; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%02d", i), "n": int32(i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	midLSN := main.HeadLSN - 10

	// Snapshot at head, then reset to the mid point: the snapshot now
	// contains discarded history and must not serve post-reset reads.
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	_, err = f.restore.ResetBranchToLSN(main.ID, midLSN)
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	// New writes after the reset.
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "post-reset", "n": int32(100)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	f.requireSnapshotMatchesFullReplay(t, main, "after reset")

	state, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.NotContains(t, state, "d15", "discarded history must not resurface via the snapshot")
	assert.Contains(t, state, "post-reset")

	// A snapshot taken after the reset is valid again and serves reads.
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "later", "n": int32(101)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	f.requireSnapshotMatchesFullReplay(t, main, "post-reset snapshot plus delta")
}

func TestSnapshot_IncrementalAndDeduplicated(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("snap-incr", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	// A stable collection that never changes again, and a hot one.
	stable := make([]bson.M, 200)
	for i := range stable {
		stable[i] = bson.M{"_id": fmt.Sprintf("s%03d", i), "payload": fmt.Sprintf("stable-%d", i)}
	}
	_, err = writer.PutMany(ctx, "stable", stable)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "hot", bson.M{"_id": "h1", "n": int32(0)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	first, err := f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	// Touch only the hot collection, snapshot again.
	_, err = writer.Put(ctx, "hot", bson.M{"_id": "h1", "n": int32(1)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	second, err := f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	chunkOf := func(snaps []*snapshot.Snapshot, coll string) []string {
		for _, s := range snaps {
			if s.Collection == coll {
				return s.ChunkIDs
			}
		}
		return nil
	}
	assert.Equal(t, chunkOf(first, "stable"), chunkOf(second, "stable"),
		"unchanged collection re-serializes to identical content-addressed chunks")
	assert.NotEqual(t, chunkOf(first, "hot"), chunkOf(second, "hot"),
		"changed collection produces new chunks")

	// Chunk store holds the stable chunks only once.
	count, err := db.Collection("wal_snapshot_chunks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, len(chunkOf(first, "stable"))+2, count,
		"stable chunks deduplicated; two distinct hot chunks")
}

func TestSnapshot_BoundsReplayDepth(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping snapshot performance canary in short mode")
	}
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("snap-perf", "main", "")
	require.NoError(t, err)

	// Build a deep history with direct batched appends (fast): 20k puts.
	const total = 20000
	const batch = 1000
	for b := 0; b < total/batch; b++ {
		entries := make([]*wal.Entry, batch)
		for i := range entries {
			docID := fmt.Sprintf("doc-%d", (b*batch+i)%5000)
			post, err := bson.Marshal(bson.M{"_id": docID, "iter": int32(b*batch + i)})
			require.NoError(t, err)
			entries[i] = &wal.Entry{
				ProjectID:  "snap-perf",
				BranchID:   main.ID,
				Operation:  wal.OpPut,
				Collection: "big",
				DocumentID: docID,
				PostImage:  post,
			}
		}
		lsns, err := f.wal.AppendBatch(entries)
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(main.ID, lsns[len(lsns)-1]))
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	fullStart := time.Now()
	full, err := f.matFull.MaterializeCollection(main, "big")
	require.NoError(t, err)
	fullElapsed := time.Since(fullStart)

	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	// A small delta on top of the snapshot.
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 50; i++ {
		_, err := writer.Put(ctx, "big", bson.M{"_id": fmt.Sprintf("delta-%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	snapStart := time.Now()
	accelerated, err := f.mat.MaterializeCollection(main, "big")
	require.NoError(t, err)
	snapElapsed := time.Since(snapStart)

	assert.Len(t, accelerated, len(full)+50)
	t.Logf("full replay of %d entries: %v; snapshot+50-delta: %v", total, fullElapsed, snapElapsed)
	// Canary, not a benchmark: the snapshot path must clearly beat full
	// replay on a 20k-entry history.
	assert.Less(t, snapElapsed, fullElapsed,
		"snapshot-backed materialization should be faster than full replay")
}

// TestSnapshot_MultiChunkParallelLoad forces a snapshot to span multiple
// chunks and requires the (parallel) load to reproduce full replay exactly.
func TestSnapshot_MultiChunkParallelLoad(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("snap-chunks", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	// ~6MB of documents (300 × 20KB) exceeds the 4MB chunk cut-off.
	payload := strings.Repeat("x", 20*1024)
	batch := make([]bson.M, 0, 50)
	for i := 0; i < 300; i++ {
		batch = append(batch, bson.M{"_id": fmt.Sprintf("doc-%03d", i), "n": int32(i), "payload": payload})
		if len(batch) == 50 {
			_, err := writer.PutMany(ctx, "bulk", batch)
			require.NoError(t, err)
			batch = batch[:0]
		}
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	snaps, err := f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	var bulk *snapshot.Snapshot
	for _, s := range snaps {
		if s.Collection == "bulk" {
			bulk = s
		}
	}
	require.NotNil(t, bulk)
	require.Greater(t, len(bulk.ChunkIDs), 1, "the snapshot must span multiple chunks for this test to mean anything")

	start := time.Now()
	accelerated, err := f.mat.MaterializeCollection(main, "bulk")
	require.NoError(t, err)
	t.Logf("parallel load of %d chunks (%d docs): %v", len(bulk.ChunkIDs), bulk.DocCount, time.Since(start))

	full, err := f.matFull.MaterializeCollection(main, "bulk")
	require.NoError(t, err)
	assert.Equal(t, full, accelerated, "parallel chunk load must equal full replay")
	assert.Len(t, accelerated, 300)
}
