package wal_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// A merge plan is computed from the documents the source branch wrote since
// its fork (merge.Service.ComputeChanged). The tests here hold that path to
// the reference that rebuilds all three states in full (ComputeFull): for
// every history they build, the two must produce the same plan.

// countingSnapshots counts snapshot loads, to show which reads touch a
// collection-sized object and which do not.
type countingSnapshots struct {
	materializer.SnapshotSource
	loads int
}

func (c *countingSnapshots) FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (map[string]bson.M, int64, bool, error) {
	state, lsn, ok, err := c.SnapshotSource.FindUsable(branch, collection, minLSN, maxLSN, readUpperBound)
	if ok {
		c.loads++
	}
	return state, lsn, ok, err
}

func sameDocument(t *testing.T, want, got bson.M, label string) {
	t.Helper()
	if want == nil || got == nil {
		require.Equal(t, want == nil, got == nil, "%s: one side is absent", label)
		return
	}
	equal, err := mongoexpr.CanonicalEqual(want, got)
	require.NoError(t, err, label)
	require.True(t, equal, "%s:\nwant %v\ngot  %v", label, want, got)
}

func requireSamePlan(t *testing.T, want, got *merge.Plan, label string) {
	t.Helper()
	require.Equal(t, want.SourceBranchID, got.SourceBranchID, label)
	require.Equal(t, want.TargetBranchID, got.TargetBranchID, label)
	require.Equal(t, want.SourceHead, got.SourceHead, label)
	require.Equal(t, want.TargetHead, got.TargetHead, label)
	require.Equal(t, want.BaseLSN, got.BaseLSN, label)

	require.Len(t, got.Changes, len(want.Changes), "%s: changes", label)
	for i, w := range want.Changes {
		g := got.Changes[i]
		at := fmt.Sprintf("%s: change %d (%s/%s)", label, i, w.Collection, w.DocumentID)
		require.Equal(t, w.Collection, g.Collection, at)
		require.Equal(t, w.DocumentID, g.DocumentID, at)
		require.Equal(t, w.Delete, g.Delete, at)
		require.Equal(t, wal.DocumentIDString(w.ID), wal.DocumentIDString(g.ID), at)
		sameDocument(t, w.Document, g.Document, at+" document")
		sameDocument(t, w.Before, g.Before, at+" before")
	}
	require.Len(t, got.Conflicts, len(want.Conflicts), "%s: conflicts", label)
	for i, w := range want.Conflicts {
		g := got.Conflicts[i]
		at := fmt.Sprintf("%s: conflict %d (%s/%s)", label, i, w.Collection, w.DocumentID)
		require.Equal(t, w.Collection, g.Collection, at)
		require.Equal(t, w.DocumentID, g.DocumentID, at)
		sameDocument(t, w.Base, g.Base, at+" base")
		sameDocument(t, w.Ours, g.Ours, at+" ours")
		sameDocument(t, w.Theirs, g.Theirs, at+" theirs")
	}
}

// requirePlansAgree checks the changed-documents path and the default entry
// point against the full reference, and returns the plan.
func requirePlansAgree(t *testing.T, svc *merge.Service, sourceID, label string) *merge.Plan {
	t.Helper()
	full, err := svc.ComputeFull(sourceID)
	require.NoError(t, err, "%s: full", label)
	changed, err := svc.ComputeChanged(sourceID)
	require.NoError(t, err, "%s: changed-documents path", label)
	requireSamePlan(t, full, changed, label+": changed vs full")
	plan, err := svc.Compute(sourceID)
	require.NoError(t, err, "%s: compute", label)
	requireSamePlan(t, full, plan, label+": compute vs full")
	return full
}

// typedIDs is a key space whose members are easy to confuse: an ObjectID and
// the string of its hex form, the same number in two widths, a string that
// reads as extended JSON, and embedded documents that differ only in field
// order.
func typedIDs() []interface{} {
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	return []interface{}{
		oid,
		oid.Hex(),
		int32(7),
		int64(7),
		"{\"$numberInt\":\"7\"}",
		bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}},
		bson.D{{Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}},
		"plain",
	}
}

// applyMixedWorkload issues seeded puts and deletes over string and typed
// IDs in a few collections.
func applyMixedWorkload(t *testing.T, rng *rand.Rand, writer *walwriter.Writer, numOps int) {
	t.Helper()
	ctx := context.Background()
	collections := []string{"users", "orders", "typed"}
	typed := typedIDs()

	for i := 0; i < numOps; i++ {
		coll := collections[rng.Intn(len(collections))]
		var id interface{} = fmt.Sprintf("doc-%d", rng.Intn(25))
		if coll == "typed" {
			id = typed[rng.Intn(len(typed))]
		}
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4:
			_, err := writer.Put(ctx, coll, bson.M{
				"_id":    id,
				"n":      int32(rng.Intn(4)), // few values: both sides often land on the same one
				"nested": bson.M{"z": rng.Intn(3), "a": []interface{}{rng.Intn(2)}},
			})
			require.NoError(t, err)
		case 5, 6:
			batch := make([]bson.M, 0, 3)
			for j := 0; j < 3; j++ {
				batch = append(batch, bson.M{"_id": fmt.Sprintf("batch-%d", rng.Intn(12)), "n": int32(rng.Intn(3))})
			}
			_, err := writer.PutMany(ctx, coll, batch)
			require.NoError(t, err)
		default:
			_, _, err := writer.Delete(ctx, coll, id)
			require.NoError(t, err)
		}
	}
}

func TestMergeChanged_MatchesFullOnRandomHistories(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	// Totals over all seeds: the generator has to reach every outcome and
	// every read path for the comparison to mean anything.
	var totalChanges, totalConflicts, totalReclaimed, loadsAfterGC int

	for seed := int64(1); seed <= 40; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			f, svc, counter := newCountingMergeFixture(t, db)
			gcService := gc.NewService(f.wal, f.branches, f.snapshots)
			project := fmt.Sprintf("merge-changed-%d", seed)

			reload := func(b *wal.Branch) *wal.Branch {
				fresh, err := f.branches.GetBranchByID(b.ID)
				require.NoError(t, err)
				return fresh
			}
			writer := func(b *wal.Branch) *walwriter.Writer {
				return walwriter.New(f.wal, f.branches, f.mat, reload(b))
			}
			snapshotHead := func(b *wal.Branch) {
				b = reload(b)
				if b.HeadLSN > b.BaseLSN {
					_, err := f.snapshots.CreateSnapshot(ctx, b.ID, b.HeadLSN)
					require.NoError(t, err)
				}
			}
			// resetSomewhere rewinds a branch to a random earlier position of
			// its own timeline, when it has one.
			resetSomewhere := func(b *wal.Branch) {
				b = reload(b)
				if b.HeadLSN-b.BaseLSN < 3 {
					return
				}
				target := b.BaseLSN + 1 + rng.Int63n(b.HeadLSN-b.BaseLSN-1)
				if b.ValidateResetTarget(target) != nil {
					return
				}
				_, err := f.restore.ResetBranchToLSN(b.ID, target)
				require.NoError(t, err)
			}

			main, err := f.branches.CreateBranch(project, "main", "")
			require.NoError(t, err)
			applyMixedWorkload(t, rng, writer(main), 40)
			if rng.Intn(2) == 0 {
				snapshotHead(main)
			}

			// The target is main, or a branch one or two levels down, so the
			// fork state is read through one to three inherited segments.
			target := main
			for level := rng.Intn(3); level > 0; level-- {
				parent := target
				target, err = f.branches.CreateBranch(project, fmt.Sprintf("mid-%d", level), parent.ID)
				require.NoError(t, err)
				applyMixedWorkload(t, rng, writer(target), 20)
				// Later work on the parent is not part of the child's history.
				applyMixedWorkload(t, rng, writer(parent), 8)
				if rng.Intn(3) == 0 {
					snapshotHead(parent)
				}
			}
			if rng.Intn(3) == 0 {
				snapshotHead(target)
			}

			if rng.Intn(4) == 0 {
				resetSomewhere(target)
				applyMixedWorkload(t, rng, writer(target), rng.Intn(8))
			}

			// The source forks from the target's head, or from a point of
			// its history — which after a reset can be history the target
			// itself no longer has.
			var source *wal.Branch
			if rng.Intn(3) == 0 {
				fresh := reload(target)
				at := fresh.BaseLSN + rng.Int63n(fresh.HistoryHighWater()-fresh.BaseLSN+1)
				source, err = f.restore.CreateBranchAtLSN(project, target.ID, "feature", at)
			} else {
				source, err = f.branches.CreateBranch(project, "feature", target.ID)
			}
			require.NoError(t, err)

			for round := 0; round < 3; round++ {
				applyMixedWorkload(t, rng, writer(source), 5+rng.Intn(15))
				if rng.Intn(4) == 0 {
					resetSomewhere(source)
					applyMixedWorkload(t, rng, writer(source), rng.Intn(6))
				}
				if rng.Intn(2) == 0 {
					applyMixedWorkload(t, rng, writer(target), rng.Intn(12))
				}
				if rng.Intn(5) == 0 {
					// A reset of the target can reach below the fork point:
					// the fork keeps the history it captured, the target no
					// longer has it.
					resetSomewhere(target)
					applyMixedWorkload(t, rng, writer(target), rng.Intn(6))
				}
				if rng.Intn(4) == 0 {
					snapshotHead(target)
				}
				requirePlansAgree(t, svc, source.ID, fmt.Sprintf("round %d", round))
			}

			// Reclaim what snapshots cover on the inherited chain. The plan
			// must not change; the changed-documents path now reads the
			// snapshots where the log no longer holds the entries.
			before := requirePlansAgree(t, svc, source.ID, "before GC")
			snapshotHead(main)
			snapshotHead(target)
			report, err := gcService.RunProject(ctx, project, zeroRetention)
			require.NoError(t, err)
			counter.loads = 0
			changed, err := svc.ComputeChanged(source.ID)
			require.NoError(t, err)
			loadsAfterGC += counter.loads
			requireSamePlan(t, before, changed, "GC changed the plan")
			after := requirePlansAgree(t, svc, source.ID, "after GC")
			totalChanges += len(after.Changes)
			totalConflicts += len(after.Conflicts)
			totalReclaimed += int(report.EntriesRemoved)
		})
	}
	require.NotZero(t, totalChanges, "no seed produced a change")
	require.NotZero(t, totalConflicts, "no seed produced a conflict")
	require.NotZero(t, totalReclaimed, "garbage collection never reclaimed an entry")
	require.NotZero(t, loadsAfterGC, "reclaimed history was never read from a snapshot")
}

// seedImported builds a main branch that looks like an imported database:
// many documents, and a snapshot at the head.
func seedImported(t *testing.T, f *snapshotFixture, project string, docs int) *wal.Branch {
	t.Helper()
	ctx := context.Background()
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	batch := make([]bson.M, 0, docs)
	for i := 0; i < docs; i++ {
		batch = append(batch, bson.M{"_id": fmt.Sprintf("p%04d", i), "price": int32(i)})
	}
	_, err = writer.PutMany(ctx, "products", batch)
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	return main
}

func newCountingMergeFixture(t *testing.T, db *mongo.Database) (*snapshotFixture, *merge.Service, *countingSnapshots) {
	t.Helper()
	f := newSnapshotFixture(t, db)
	counter := &countingSnapshots{SnapshotSource: f.snapshots}
	f.mat.SetSnapshotSource(counter)
	return f, merge.NewService(db, f.wal, f.branches, f.mat, db.Client()), counter
}

// The common case — a sandbox forked from a large parent, a few documents
// changed — must be answered from those documents' own histories: no
// snapshot of the parent is loaded, which is the collection-sized read.
func TestMergeChanged_ReadsOnlyChangedDocuments(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, counter := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-small", 600)

	feature, err := f.branches.CreateBranch("merge-changed-small", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(15)}) // update
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "brand-new", "price": int32(30)}) // insert
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "products", "p0002") // delete
	require.NoError(t, err)
	_, err = fw.Put(ctx, "notes", bson.M{"_id": "n1", "text": "a collection the parent never had"})
	require.NoError(t, err)
	// The parent moved on too: one overlapping document, one unrelated.
	mw := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(99)})
	require.NoError(t, err)
	_, err = mw.Put(ctx, "products", bson.M{"_id": "p0500", "price": int32(1)})
	require.NoError(t, err)

	// The writes above read pre-images; count from here.
	counter.loads = 0
	changed, err := svc.ComputeChanged(feature.ID)
	require.NoError(t, err)
	assert.Zero(t, counter.loads, "no snapshot is loaded while the log still holds every entry")
	require.Len(t, changed.Changes, 3, "insert, delete and the new collection's document")
	require.Len(t, changed.Conflicts, 1, "both sides changed p0001")
	assert.Equal(t, "p0001", changed.Conflicts[0].DocumentID)
	assert.EqualValues(t, 1, changed.Conflicts[0].Base["price"])
	assert.EqualValues(t, 99, changed.Conflicts[0].Ours["price"])
	assert.EqualValues(t, 15, changed.Conflicts[0].Theirs["price"])

	full, err := svc.ComputeFull(feature.ID)
	require.NoError(t, err)
	assert.NotZero(t, counter.loads, "the full reference does load the parent's snapshot")
	requireSamePlan(t, full, changed, "changed vs full")
}

// Once garbage collection has deleted the parent's entries, a document
// without an entry may still have had one. The snapshot is then the record
// of its state, and the plan is unchanged.
func TestMergeChanged_ReclaimedParentHistory(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, counter := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-gc", 200)

	feature, err := f.branches.CreateBranch("merge-changed-gc", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(1000)})
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "products", "p0004")
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "added", "price": int32(5)})
	require.NoError(t, err)

	before := requirePlansAgree(t, svc, feature.ID, "before GC")
	require.Len(t, before.Changes, 3)

	report, err := gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, "merge-changed-gc", zeroRetention)
	require.NoError(t, err)
	require.NotZero(t, report.EntriesRemoved, "the parent's imported entries were reclaimed")

	counter.loads = 0
	changed, err := svc.ComputeChanged(feature.ID)
	require.NoError(t, err)
	assert.NotZero(t, counter.loads, "reclaimed history is read from the snapshot")
	requireSamePlan(t, before, changed, "after GC")
	requirePlansAgree(t, svc, feature.ID, "after GC")
}

// When garbage collection has deleted entries of the source branch itself,
// its log no longer names every document it changed. The changed-documents
// path says so, and Compute answers from a full replay.
func TestMergeChanged_ReclaimedSourceHistoryFallsBack(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-src-gc", 50)

	feature, err := f.branches.CreateBranch("merge-changed-src-gc", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(77)})
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "added", "price": int32(5)})
	require.NoError(t, err)
	feature, err = f.branches.GetBranchByID(feature.ID)
	require.NoError(t, err)

	want, err := svc.ComputeFull(feature.ID)
	require.NoError(t, err)
	require.Len(t, want.Changes, 2)

	_, err = f.snapshots.CreateSnapshot(ctx, feature.ID, feature.HeadLSN)
	require.NoError(t, err)
	_, err = gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, "merge-changed-src-gc", zeroRetention)
	require.NoError(t, err)
	require.Zero(t, countBranchEntriesIn(t, f, feature.ID, "products"), "the source's own entries were reclaimed")

	_, err = svc.ComputeChanged(feature.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, merge.ErrFullReplayRequired), "got: %v", err)

	plan, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	requireSamePlan(t, want, plan, "compute after the source was reclaimed")
}

func countBranchEntriesIn(t *testing.T, f *snapshotFixture, branchID, collection string) int {
	t.Helper()
	entries, err := f.wal.GetBranchEntries(branchID, collection, 0, int64(1)<<62)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.IsData() {
			n++
		}
	}
	return n
}

// A reset takes writes off the branch's timeline. A document touched only by
// discarded writes is not a change; one written again afterwards is judged
// by the later write.
func TestMergeChanged_SourceReset(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-reset", 20)

	feature, err := f.branches.CreateBranch("merge-changed-reset", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	keep, err := fw.Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(11)})
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0002", "price": int32(22)}) // discarded below
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(33)}) // discarded, then rewritten
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "products", "p0001") // discarded: the kept put stands
	require.NoError(t, err)

	feature, err = f.restore.ResetBranchToLSN(feature.ID, keep)
	require.NoError(t, err)
	fw = walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(333)})
	require.NoError(t, err)

	plan := requirePlansAgree(t, svc, feature.ID, "after reset")
	require.Len(t, plan.Changes, 2)
	assert.Equal(t, "p0001", plan.Changes[0].DocumentID)
	assert.False(t, plan.Changes[0].Delete, "the discarded delete does not count")
	assert.EqualValues(t, 11, plan.Changes[0].Document["price"])
	assert.Equal(t, "p0003", plan.Changes[1].DocumentID)
	assert.EqualValues(t, 333, plan.Changes[1].Document["price"])
}

// An ObjectID and the string of its hex form are two documents. A change to
// one must be read against the history of that one.
func TestMergeChanged_LookalikeIDs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)

	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	main, err := f.branches.CreateBranch("merge-changed-ids", "main", "")
	require.NoError(t, err)
	mw := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.PutMany(ctx, "docs", []bson.M{
		{"_id": oid, "who": "objectid"},
		{"_id": oid.Hex(), "who": "string"},
		{"_id": int32(7), "who": "int32"},
		{"_id": int64(7), "who": "int64"},
	})
	require.NoError(t, err)

	feature, err := f.branches.CreateBranch("merge-changed-ids", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "docs", bson.M{"_id": oid.Hex(), "who": "string, edited"})
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "docs", int64(7))
	require.NoError(t, err)
	// The parent edits the look-alikes, which must not read as conflicts.
	mw = walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.Put(ctx, "docs", bson.M{"_id": oid, "who": "objectid, edited on main"})
	require.NoError(t, err)
	_, err = mw.Put(ctx, "docs", bson.M{"_id": int32(7), "who": "int32, edited on main"})
	require.NoError(t, err)

	plan := requirePlansAgree(t, svc, feature.ID, "lookalike ids")
	assert.Empty(t, plan.Conflicts)
	require.Len(t, plan.Changes, 2)
	for _, c := range plan.Changes {
		switch id := c.ID.(type) {
		case string:
			assert.Equal(t, oid.Hex(), id)
			assert.Equal(t, "string, edited", c.Document["who"])
			assert.Equal(t, "string", c.Before["who"])
		case int64:
			assert.True(t, c.Delete)
			assert.Equal(t, "int64", c.Before["who"])
		default:
			t.Fatalf("unexpected change for _id %#v", c.ID)
		}
	}
}

// A branch with no writes of its own has nothing to merge, whatever its
// parent did.
func TestMergeChanged_EmptySource(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, counter := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-empty", 30)
	feature, err := f.branches.CreateBranch("merge-changed-empty", "feature", main.ID)
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, main).Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(5)})
	require.NoError(t, err)

	counter.loads = 0
	plan, err := svc.ComputeChanged(feature.ID)
	require.NoError(t, err)
	assert.Empty(t, plan.Changes)
	assert.Empty(t, plan.Conflicts)
	assert.Zero(t, counter.loads)
	requirePlansAgree(t, svc, feature.ID, "empty source")
}

// downgradeToLegacyKeys rewrites a branch's stored entries the way releases
// before typed document keys wrote them: the key is the legacy string form
// of the _id and carries no key version. In that form an ObjectID and the
// string of its hex share one stored key. stripPreImage additionally removes
// the pre-image of the delete entries of the named document.
func downgradeToLegacyKeys(t *testing.T, db *mongo.Database, f *snapshotFixture, branchID string, stripPreImage interface{}) {
	t.Helper()
	ctx := context.Background()
	entries, err := f.wal.GetBranchEntries(branchID, "", 0, int64(1)<<62)
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsData() {
			continue
		}
		image := e.PostImage
		if e.Operation == wal.OpDelete {
			image = e.PreImage
		}
		id, err := wal.DocumentIDFromImage(image)
		require.NoError(t, err)
		update := bson.M{
			"$set":   bson.M{"document_id": wal.LegacyDocumentIDString(id)},
			"$unset": bson.M{"key_v": ""},
		}
		if e.Operation == wal.OpDelete && stripPreImage != nil && wal.DocumentIDString(id) == wal.DocumentIDString(stripPreImage) {
			update["$unset"] = bson.M{"key_v": "", "pre": ""}
		}
		allowOldFormatRows(t, db)
		res, err := db.Collection("wal_log").UpdateOne(ctx, bson.M{"_id": e.ID}, update)
		require.NoError(t, err)
		require.EqualValues(t, 1, res.MatchedCount)
	}
}

// History written before typed keys is declined by the changed-documents
// path as a whole, and the plan comes from a full replay, which attributes
// each entry by the _id in its image.
func TestMergeChanged_LegacyStoredKeys(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)

	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	main, err := f.branches.CreateBranch("merge-changed-legacy", "main", "")
	require.NoError(t, err)
	mw := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.PutMany(ctx, "docs", []bson.M{
		{"_id": oid, "who": "objectid"},
		{"_id": oid.Hex(), "who": "string"},
		{"_id": int32(7), "who": "int32"},
		{"_id": int64(7), "who": "int64"},
		{"_id": "plain", "who": "plain"},
		{"_id": "gone-on-main", "who": "deleted by the parent before the fork"},
	})
	require.NoError(t, err)
	_, _, err = mw.Delete(ctx, "docs", "gone-on-main")
	require.NoError(t, err)

	feature, err := f.branches.CreateBranch("merge-changed-legacy", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "docs", bson.M{"_id": oid.Hex(), "who": "string, edited"})
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "docs", int64(7))
	require.NoError(t, err)
	_, err = fw.Put(ctx, "docs", bson.M{"_id": "plain", "who": "plain, edited"})
	require.NoError(t, err)
	_, err = fw.Put(ctx, "docs", bson.M{"_id": "gone-on-main", "who": "created again on the branch"})
	require.NoError(t, err)
	mw = walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.Put(ctx, "docs", bson.M{"_id": oid, "who": "objectid, edited on main"})
	require.NoError(t, err)
	_, err = mw.Put(ctx, "docs", bson.M{"_id": int32(7), "who": "int32, edited on main"})
	require.NoError(t, err)

	want := requirePlansAgree(t, svc, feature.ID, "current keys")
	require.Len(t, want.Changes, 4)
	require.Empty(t, want.Conflicts)

	// The parent's delete keeps no pre-image: with a plain string key it
	// still names exactly one document.
	downgradeToLegacyKeys(t, db, f, main.ID, "gone-on-main")
	downgradeToLegacyKeys(t, db, f, feature.ID, nil)

	// Keys of that time take several stored forms; the changed-documents
	// path does not try to read them one document at a time.
	_, err = svc.ComputeChanged(feature.ID)
	require.Error(t, err)
	full, err := svc.ComputeFull(feature.ID)
	require.NoError(t, err)
	got, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	requireSamePlan(t, full, got, "legacy keys: compute vs full")
	requireSamePlan(t, want, got, "legacy keys vs current keys")
}

// A legacy delete without a pre-image does not say which document it removed
// when its key reads as more than one _id. Neither path may guess: the
// changed-documents path declines, and the answer is whatever a full replay
// gives.
func TestMergeChanged_AmbiguousLegacyDeleteIsNotGuessed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")

	build := func(project string) (*snapshotFixture, *merge.Service, *wal.Branch, *wal.Branch) {
		f, svc, _ := newCountingMergeFixture(t, db)
		main, err := f.branches.CreateBranch(project, "main", "")
		require.NoError(t, err)
		mw := walwriter.New(f.wal, f.branches, f.mat, main)
		_, err = mw.PutMany(ctx, "docs", []bson.M{
			{"_id": oid, "who": "objectid"},
			{"_id": "other", "who": "other"},
		})
		require.NoError(t, err)
		feature, err := f.branches.CreateBranch(project, "feature", main.ID)
		require.NoError(t, err)
		return f, svc, main, feature
	}

	t.Run("on the source branch", func(t *testing.T) {
		f, svc, _, feature := build("merge-changed-ambiguous-src")
		fw := walwriter.New(f.wal, f.branches, f.mat, feature)
		_, _, err := fw.Delete(ctx, "docs", oid)
		require.NoError(t, err)
		downgradeToLegacyKeys(t, db, f, feature.ID, oid)

		_, err = svc.ComputeChanged(feature.ID)
		require.Error(t, err)
		assert.True(t, errors.Is(err, merge.ErrFullReplayRequired), "got: %v", err)

		// One document matches the legacy key, so a full replay resolves it.
		full, err := svc.ComputeFull(feature.ID)
		require.NoError(t, err)
		plan, err := svc.Compute(feature.ID)
		require.NoError(t, err)
		requireSamePlan(t, full, plan, "fallback")
		require.Len(t, plan.Changes, 1)
		assert.True(t, plan.Changes[0].Delete)
	})

	t.Run("on the inherited chain", func(t *testing.T) {
		f, svc, main, feature := build("merge-changed-ambiguous-parent")
		// After the fork the parent deletes the ObjectID document; the
		// branch edits it. A full replay sees delete-versus-edit.
		mw := walwriter.New(f.wal, f.branches, f.mat, main)
		_, _, err := mw.Delete(ctx, "docs", oid)
		require.NoError(t, err)
		fw := walwriter.New(f.wal, f.branches, f.mat, feature)
		_, err = fw.Put(ctx, "docs", bson.M{"_id": oid, "who": "objectid, edited"})
		require.NoError(t, err)
		downgradeToLegacyKeys(t, db, f, main.ID, oid)

		_, err = svc.ComputeChanged(feature.ID)
		require.Error(t, err, "the delete's key reads as an ObjectID or a string")

		full, err := svc.ComputeFull(feature.ID)
		require.NoError(t, err)
		plan, err := svc.Compute(feature.ID)
		require.NoError(t, err)
		requireSamePlan(t, full, plan, "fallback")
		require.Len(t, plan.Conflicts, 1)
		assert.Nil(t, plan.Conflicts[0].Ours, "the parent's delete")
	})
}

// Garbage collection in releases up to 2.1.5 deleted entries without
// recording a reclaim floor, and its cutoff could lie below the snapshot that
// covered it. On such a database a document without an entry may have had
// one. The manifests of that time mark how far the deletion can reach, and
// below that mark the snapshot answers, exactly as in a full replay.
func TestMergeChanged_ReclaimedWithoutFloor(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, counter := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-nofloor", 200)

	// What a 2.1.5 run left behind: a manifest without visibility rules, the
	// older part of the entries gone, the newer part still there, no floor.
	res, err := db.Collection("wal_snapshots").UpdateMany(ctx, bson.M{"branch_id": main.ID}, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.ModifiedCount)
	cutoff := main.HeadLSN - 50
	removed, err := f.wal.DeleteDataEntriesUpTo(main.ID, "products", cutoff)
	require.NoError(t, err)
	require.NotZero(t, removed)
	require.NotZero(t, countBranchEntriesIn(t, f, main.ID, "products"), "entries above the cutoff remain")
	floors, err := f.wal.ReclaimFloors(main.ID)
	require.NoError(t, err)
	require.Empty(t, floors)

	feature, err := f.branches.CreateBranch("merge-changed-nofloor", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, _, err = fw.Delete(ctx, "products", "p0001") // its entry is gone
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0002", "price": int32(-2)}) // its entry is gone
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0199", "price": int32(-199)}) // its entry remains
	require.NoError(t, err)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "added", "price": int32(1)})
	require.NoError(t, err)
	// The parent edits a document whose import entry is gone, too.
	_, err = walwriter.New(f.wal, f.branches, f.mat, main).Put(ctx, "products", bson.M{"_id": "p0002", "price": int32(2000)})
	require.NoError(t, err)

	counter.loads = 0
	changed, err := svc.ComputeChanged(feature.ID)
	require.NoError(t, err)
	assert.NotZero(t, counter.loads, "history that may be gone is read from the snapshot")
	full, err := svc.ComputeFull(feature.ID)
	require.NoError(t, err)
	requireSamePlan(t, full, changed, "changed vs full")

	require.Len(t, changed.Changes, 3)
	assert.Equal(t, "added", changed.Changes[0].DocumentID)
	assert.Equal(t, "p0001", changed.Changes[1].DocumentID)
	assert.True(t, changed.Changes[1].Delete, "the delete of a document whose entry was reclaimed is part of the plan")
	assert.EqualValues(t, 1, changed.Changes[1].Before["price"])
	assert.Equal(t, "p0199", changed.Changes[2].DocumentID)
	assert.EqualValues(t, 199, changed.Changes[2].Before["price"])
	require.Len(t, changed.Conflicts, 1)
	assert.Equal(t, "p0002", changed.Conflicts[0].DocumentID)
	assert.EqualValues(t, 2, changed.Conflicts[0].Base["price"], "the fork state comes from the snapshot")
}

// An entry at or below a reclaim floor is not evidence: garbage collection
// records the floor and then deletes, so such an entry is one a deletion has
// not reached yet, and a newer entry of the same document may already be
// gone. A replay never applies it; neither does the point read.
func TestMergeChanged_EntryBelowFloorIsNotTrusted(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-stale", 20)

	// Keep the stored form of p0003's first entry, then move the document on.
	var stale bson.Raw
	stale, err := db.Collection("wal_log").FindOne(ctx, bson.M{"branch_id": main.ID, "document_id": "p0003"}).Raw()
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, main).Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(3000)})
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	_, err = gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, "merge-changed-stale", zeroRetention)
	require.NoError(t, err)
	require.Zero(t, countBranchEntriesIn(t, f, main.ID, "products"))
	// A deletion that stopped half way: the old entry is back, the newer one
	// is not.
	allowOldFormatRows(t, db)
	_, err = db.Collection("wal_log").InsertOne(ctx, stale)
	require.NoError(t, err)

	feature, err := f.branches.CreateBranch("merge-changed-stale", "feature", main.ID)
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, feature).Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(-3)})
	require.NoError(t, err)

	plan := requirePlansAgree(t, svc, feature.ID, "stale entry below the floor")
	require.Len(t, plan.Changes, 1)
	assert.EqualValues(t, 3000, plan.Changes[0].Before["price"], "the parent's state is the snapshot's, not the stale entry's")
}

// A schema-v1 update names no document, so a lookup by document key cannot
// see it. The point read has to notice it is there and decline; what the
// plan is then is the full replay's business — it refuses until the WAL is
// migrated, unless a snapshot already covers the entry.
func TestMergeChanged_UnkeyedLegacyEntryIsNotSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	build := func(project string, snapshot bool) (*merge.Service, *wal.Branch) {
		f, svc, _ := newCountingMergeFixture(t, db)
		main, err := f.branches.CreateBranch(project, "main", "")
		require.NoError(t, err)
		_, err = walwriter.New(f.wal, f.branches, f.mat, main).PutMany(ctx, "products", []bson.M{
			{"_id": "p0001", "price": int32(1)}, {"_id": "p0005", "price": int32(5)},
		})
		require.NoError(t, err)
		main, err = f.branches.GetBranchByID(main.ID)
		require.NoError(t, err)
		if snapshot {
			_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
			require.NoError(t, err)
		}
		feature, err := f.branches.CreateBranch(project, "feature", main.ID)
		require.NoError(t, err)
		_, err = walwriter.New(f.wal, f.branches, f.mat, feature).Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(-1)})
		require.NoError(t, err)
		_, err = svc.ComputeChanged(feature.ID)
		require.NoError(t, err)

		// Turn one of the parent's entries into what 1.x wrote for an update.
		allowOldFormatRows(t, db)
		res, err := db.Collection("wal_log").UpdateOne(ctx,
			bson.M{"branch_id": main.ID, "document_id": "p0005"},
			bson.M{"$set": bson.M{"operation": "update", "v": 1}, "$unset": bson.M{"document_id": "", "key_v": ""}})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.ModifiedCount)
		return svc, feature
	}

	t.Run("without a snapshot the plan is refused", func(t *testing.T) {
		svc, feature := build("merge-changed-v1", false)
		_, err := svc.ComputeChanged(feature.ID)
		require.Error(t, err, "an entry that names no document must not be read past")
		_, err = svc.ComputeFull(feature.ID)
		require.Error(t, err)
		_, err = svc.Compute(feature.ID)
		require.Error(t, err, "refused, as before, until the WAL is migrated")
	})

	t.Run("with a snapshot the full replay answers", func(t *testing.T) {
		svc, feature := build("merge-changed-v1-snap", true)
		_, err := svc.ComputeChanged(feature.ID)
		require.Error(t, err, "an entry that names no document must not be read past")
		full, err := svc.ComputeFull(feature.ID)
		require.NoError(t, err)
		plan, err := svc.Compute(feature.ID)
		require.NoError(t, err)
		requireSamePlan(t, full, plan, "fallback")
		require.Len(t, plan.Changes, 1)
	})
}

// Capture in 2.0.x wrote deletes without a pre-image under legacy keys. For
// an _id of true, false or null that key is a plain word, which is not
// refused as ambiguous, and it differs from the document's current key. The
// delete still has to count for that document.
func TestMergeChanged_LegacyImagelessDeleteOfBooleanID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)

	main, err := f.branches.CreateBranch("merge-changed-bool", "main", "")
	require.NoError(t, err)
	mw := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mw.PutMany(ctx, "docs", []bson.M{{"_id": true, "who": "boolean"}, {"_id": "other", "who": "other"}})
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("merge-changed-bool", "feature", main.ID)
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, feature).Put(ctx, "docs", bson.M{"_id": true, "who": "boolean, edited"})
	require.NoError(t, err)
	_, _, err = walwriter.New(f.wal, f.branches, f.mat, main).Delete(ctx, "docs", true)
	require.NoError(t, err)
	downgradeToLegacyKeys(t, db, f, main.ID, true)

	_, err = svc.ComputeChanged(feature.ID)
	require.Error(t, err, "legacy keys are not read one document at a time")
	full, err := svc.ComputeFull(feature.ID)
	require.NoError(t, err)
	plan, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	requireSamePlan(t, full, plan, "legacy delete of _id true")
	require.Empty(t, plan.Changes)
	require.Len(t, plan.Conflicts, 1, "the parent deleted the document the branch edited")
	assert.Nil(t, plan.Conflicts[0].Ours)

	// The single-document read used by writers agrees.
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	doc, err := f.mat.MaterializeDocumentAtLSN(main, "docs", wal.DocumentIDString(true), main.HeadLSN)
	require.NoError(t, err)
	assert.Nil(t, doc)
}

// A delete captured without a pre-image by a current release carries the
// document's own key. It removes that document and not a look-alike.
func TestMergeChanged_ImagelessDeleteWithCurrentKey(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")

	main, err := f.branches.CreateBranch("merge-changed-nopre", "main", "")
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, main).PutMany(ctx, "docs", []bson.M{
		{"_id": oid, "who": "objectid"},
		{"_id": oid.Hex(), "who": "string"},
		{"_id": "third", "who": "third"},
	})
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("merge-changed-nopre", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "docs", bson.M{"_id": oid.Hex(), "who": "string, edited"})
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "docs", "third")
	require.NoError(t, err)
	_, _, err = walwriter.New(f.wal, f.branches, f.mat, main).Delete(ctx, "docs", oid)
	require.NoError(t, err)
	// Drop the pre-images of both deletes; the keys stay current.
	allowOldFormatRows(t, db)
	res, err := db.Collection("wal_log").UpdateMany(ctx, bson.M{"operation": "delete"}, bson.M{"$unset": bson.M{"pre": ""}})
	require.NoError(t, err)
	require.EqualValues(t, 2, res.ModifiedCount)

	plan := requirePlansAgree(t, svc, feature.ID, "image-less deletes")
	require.Empty(t, plan.Conflicts, "the parent deleted the ObjectID document, not the string one")
	require.Len(t, plan.Changes, 2)
}

// The same unrecorded deletion on the branch being merged: its log may no
// longer name every document it changed, so the changed-documents path
// declines and the plan comes from a full replay.
func TestMergeChanged_SourceReclaimedWithoutFloor(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-src-nofloor", 20)

	feature, err := f.branches.CreateBranch("merge-changed-src-nofloor", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "products", bson.M{"_id": "p0001", "price": int32(-1)})
	require.NoError(t, err)
	_, _, err = fw.Delete(ctx, "products", "p0002")
	require.NoError(t, err)
	feature, err = f.branches.GetBranchByID(feature.ID)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, feature.ID, feature.HeadLSN)
	require.NoError(t, err)
	_, err = walwriter.New(f.wal, f.branches, f.mat, feature).Put(ctx, "products", bson.M{"_id": "p0003", "price": int32(-3)})
	require.NoError(t, err)
	want := requirePlansAgree(t, svc, feature.ID, "before")
	require.Len(t, want.Changes, 3)

	// A 2.1.5 run: legacy manifest, entries up to it gone, no floor.
	_, err = db.Collection("wal_snapshots").UpdateMany(ctx, bson.M{"branch_id": feature.ID}, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)
	removed, err := f.wal.DeleteDataEntriesUpTo(feature.ID, "products", feature.HeadLSN)
	require.NoError(t, err)
	require.EqualValues(t, 2, removed)

	_, err = svc.ComputeChanged(feature.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, merge.ErrFullReplayRequired), "got: %v", err)
	plan, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	requireSamePlan(t, want, plan, "after unrecorded deletion on the source")
}

// More documents than one lookup batch, several versions of each on both
// sides, and a plan that must still be exact.
func TestMergeChanged_ManyDocumentsAndVersions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, counter := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-many", 1500)

	feature, err := f.branches.CreateBranch("merge-changed-many", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	mw := walwriter.New(f.wal, f.branches, f.mat, main)
	for round := int32(1); round <= 3; round++ {
		branchDocs := make([]bson.M, 0, 1200)
		for i := 0; i < 1200; i++ {
			branchDocs = append(branchDocs, bson.M{"_id": fmt.Sprintf("p%04d", i), "price": -round})
		}
		_, err = fw.PutMany(ctx, "products", branchDocs)
		require.NoError(t, err)
		// The parent rewrites a hot set, overlapping the branch's.
		parentDocs := make([]bson.M, 0, 100)
		for i := 1150; i < 1250; i++ {
			parentDocs = append(parentDocs, bson.M{"_id": fmt.Sprintf("p%04d", i), "price": 1000 * round})
		}
		_, err = mw.PutMany(ctx, "products", parentDocs)
		require.NoError(t, err)
	}

	counter.loads = 0
	plan, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	assert.Zero(t, counter.loads, "Compute, the entry point the CLI and API use, took the changed-documents path")
	require.Len(t, plan.Changes, 1150)
	require.Len(t, plan.Conflicts, 50)
	assert.EqualValues(t, -3, plan.Changes[0].Document["price"], "the newest version on the branch")
	assert.EqualValues(t, 3000, plan.Conflicts[0].Ours["price"], "the newest version on the parent")
	requirePlansAgree(t, svc, feature.ID, "many documents")
}

// A branch whose own log is very long is read as states instead.
func TestMergeChanged_LongSourceLogFallsBack(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	f, svc, _ := newCountingMergeFixture(t, db)
	main := seedImported(t, f, "merge-changed-long", 10)
	feature, err := f.branches.CreateBranch("merge-changed-long", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	for i := int32(0); i < 6; i++ {
		_, err = fw.Put(ctx, "products", bson.M{"_id": "p0001", "price": -i})
		require.NoError(t, err)
	}

	limit := merge.MaxChangedEntries
	merge.MaxChangedEntries = 5
	t.Cleanup(func() { merge.MaxChangedEntries = limit })
	_, err = svc.ComputeChanged(feature.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, merge.ErrFullReplayRequired), "got: %v", err)
	plan, err := svc.Compute(feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	assert.EqualValues(t, -5, plan.Changes[0].Document["price"])
}

// Entries written by live capture: a checked-out parent fed by its change
// stream, with updates, a replace and a delete made through the driver.
func TestMergeChanged_CapturedHistory(t *testing.T) {
	f := newIngestFixture(t, "merge-changed-live")
	svc := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	docs := f.physical.Collection("docs")
	_, err := docs.InsertMany(ctx, []interface{}{
		bson.M{"_id": "a", "v": int32(1)}, bson.M{"_id": "b", "v": int32(1)},
		bson.M{"_id": "c", "v": int32(1)}, bson.M{"_id": primitive.NewObjectID(), "v": int32(1)},
	})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 4)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("merge-changed-live", "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	for _, id := range []string{"a", "b", "c"} {
		_, err = fw.Put(ctx, "docs", bson.M{"_id": id, "v": int32(-1)})
		require.NoError(t, err)
	}
	_, err = fw.Put(ctx, "docs", bson.M{"_id": "new", "v": int32(-1)})
	require.NoError(t, err)

	// The parent moves on through the driver.
	_, err = docs.UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$set": bson.M{"v": int32(2)}, "$unset": bson.M{"gone": ""}})
	require.NoError(t, err)
	_, err = docs.ReplaceOne(ctx, bson.M{"_id": "b"}, bson.M{"v": int32(-1)}) // the branch's own value
	require.NoError(t, err)
	_, err = docs.DeleteOne(ctx, bson.M{"_id": "c"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 7)

	plan := requirePlansAgree(t, svc, feature.ID, "captured history")
	require.Len(t, plan.Changes, 1, "only the new document merges cleanly")
	require.Len(t, plan.Conflicts, 2, "a: both edited; c: deleted by the parent, edited by the branch; b: same value on both sides")
}
