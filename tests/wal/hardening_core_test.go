package wal_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/merge"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestHardeningTypedIDsMergeAndSnapshot(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	main, err := f.branches.CreateBranch("typed-merge", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	ids := []interface{}{oid, oid.Hex(), int32(42), int64(43), bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}}
	for _, id := range ids {
		_, err = w.Put(ctx, "docs", bson.M{"_id": id, "v": "base"})
		require.NoError(t, err)
	}
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	state, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	require.Len(t, state, len(ids))
	for _, id := range ids {
		require.Contains(t, state, wal.DocumentIDString(id))
	}
	feature, err := f.branches.CreateBranch(main.ProjectID, "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	for _, id := range ids[1:] {
		_, deleted, err := fw.Delete(ctx, "docs", id)
		require.NoError(t, err)
		require.True(t, deleted)
	}
	svc := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	plan, err := svc.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = svc.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	state, err = f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	require.Len(t, state, 1)
	require.Contains(t, state, wal.DocumentIDString(oid))
}

func TestHardeningWriterPreImagesAfterSnapshotGCAndRepeatedIDs(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	main, err := f.branches.CreateBranch("preimages", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	first, err := w.Put(ctx, "docs", bson.M{"_id": "d", "v": int32(1)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	_, err = f.wal.DeleteDataEntriesUpTo(main.ID, "docs", first)
	require.NoError(t, err)
	lsns, err := w.PutMany(ctx, "docs", []bson.M{{"_id": "d", "v": int32(2)}, {"_id": "d", "v": int32(3)}})
	require.NoError(t, err)
	for i, lsn := range lsns {
		e, err := f.wal.GetEntry(main.ProjectID, lsn)
		require.NoError(t, err)
		var pre bson.M
		require.NoError(t, bson.Unmarshal(e.PreImage, &pre))
		require.Equal(t, int32(i+1), pre["v"])
	}
}

func TestHardeningLegacyMergePlanRetainsNumericDeleteID(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	main, err := f.branches.CreateBranch("legacy-merge", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = w.Put(ctx, "docs", bson.M{"_id": int32(42), "v": "base"})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	feature, err := f.branches.CreateBranch(main.ProjectID, "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, _, err = fw.Delete(ctx, "docs", int32(42))
	require.NoError(t, err)
	svc := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	plan, err := svc.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = db.Collection("wal_merge_plans").UpdateOne(ctx, bson.M{"_id": plan.ID}, bson.M{
		"$unset": bson.M{"format_version": ""},
		"$set":   bson.M{"changes": bson.A{bson.M{"collection": "docs", "document_id": wal.LegacyDocumentIDString(int32(42)), "delete": true}}},
	})
	require.NoError(t, err)
	_, err = svc.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	state, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	require.Empty(t, state)
}

func TestHardeningConcurrentMergeAppliesOnce(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-once")
	ctx := context.Background()
	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "feature"})
	require.NoError(t, err)
	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := f.merge.Apply(ctx, plan.ID, ""); results <- err }()
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	entries, err := f.wal.GetBranchEntries(f.main.ID, "", 0, 1<<60)
	require.NoError(t, err)
	markers := 0
	for _, e := range entries {
		if e.Operation == wal.OpMerge {
			markers++
		}
	}
	require.Equal(t, 1, markers)
}

func TestHardeningPhysicalMergeRollsBackAllDocuments(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	main, err := f.branches.CreateBranch("merge-rollback", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	for _, coll := range []string{"alpha", "zzreject"} {
		_, err = w.Put(ctx, coll, bson.M{"_id": "d", "v": "base"})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	feature, err := f.branches.CreateBranch(main.ProjectID, "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = fw.Put(ctx, "alpha", bson.M{"_id": "d", "v": "good"})
	require.NoError(t, err)
	_, err = fw.Put(ctx, "zzreject", bson.M{"_id": "d", "v": "invalid"})
	require.NoError(t, err)
	co := checkout.NewService(db.Client(), db, f.branches, f.mat)
	info, err := co.Checkout(ctx, main.ID)
	require.NoError(t, err)
	physical := db.Client().Database(info.PhysicalDB)
	t.Cleanup(func() { _ = physical.Drop(context.Background()) })
	require.NoError(t, physical.RunCommand(ctx, bson.D{{Key: "collMod", Value: "zzreject"}, {Key: "validator", Value: bson.M{"v": bson.M{"$ne": "invalid"}}}}).Err())
	svc := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	plan, err := svc.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = svc.Apply(ctx, plan.ID, "")
	require.Error(t, err)
	var doc bson.M
	require.NoError(t, physical.Collection("alpha").FindOne(ctx, bson.M{"_id": "d"}).Decode(&doc))
	require.Equal(t, "base", doc["v"])
	stored, err := svc.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, merge.StatusPending, stored.Status)
	// A write that has not reached the WAL also prevents a stale overwrite.
	_, err = physical.Collection("alpha").UpdateOne(ctx, bson.M{"_id": "d"}, bson.M{"$set": bson.M{"v": "newer"}})
	require.NoError(t, err)
	_, err = svc.Apply(ctx, plan.ID, "")
	require.ErrorContains(t, err, "physical document")
}

func TestHardeningResetCASAndLiveGuard(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	main, err := f.branches.CreateBranch("reset-cas", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	first, err := w.Put(ctx, "docs", bson.M{"_id": "d", "v": 1})
	require.NoError(t, err)
	stale, err := f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	last, err := w.Put(ctx, "docs", bson.M{"_id": "d", "v": 2})
	require.NoError(t, err)
	require.Error(t, f.branches.ResetHead(stale, first))
	fresh, err := f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	require.Equal(t, last, fresh.HeadLSN)
	require.Empty(t, fresh.DiscardedRanges)
	_, err = f.restore.ResetBranchToLSN(main.ID, first)
	require.NoError(t, err)
	fresh, _ = f.branches.GetBranchByID(main.ID)
	require.Equal(t, first, fresh.HeadLSN)
	require.Equal(t, []wal.LSNRange{{From: first + 1, To: last}}, fresh.DiscardedRanges)
	co := checkout.NewService(db.Client(), db, f.branches, f.mat)
	info, err := co.Checkout(ctx, main.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })
	_, err = f.restore.ResetBranchToLSN(main.ID, first)
	require.ErrorContains(t, err, "checked-out")
	_, err = w.Put(ctx, "docs", bson.M{"_id": "d", "v": 3})
	require.ErrorContains(t, err, "checked out")
}

type pausedDeleteStore struct {
	snapshot.ChunkStore
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *pausedDeleteStore) Delete(ctx context.Context, ids []string) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.ChunkStore.Delete(ctx, ids)
}

func TestHardeningSnapshotPublicationWaitsForConcurrentGC(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	main, err := f.branches.CreateBranch("snapshot-lock", "main", "")
	require.NoError(t, err)
	w := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = w.Put(ctx, "docs", bson.M{"_id": "d", "v": 1})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	baseStore := snapshot.NewMongoChunkStore(db)
	store := &pausedDeleteStore{ChunkStore: baseStore, entered: make(chan struct{}), resume: make(chan struct{})}
	gcSnapshots, err := snapshot.NewServiceWithStore(db, f.branches, materializer.NewService(f.wal, f.branches), store)
	require.NoError(t, err)
	_, err = gcSnapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	// An independent service represents a second process sharing the store.
	otherMat := materializer.NewService(f.wal, f.branches)
	other, err := snapshot.NewServiceWithStore(db, f.branches, otherMat, baseStore)
	require.NoError(t, err)
	gcDone := make(chan error, 1)
	go func() { _, _, err := gcSnapshots.CleanupBranch(ctx, main.ID); gcDone <- err }()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	publicationDone := make(chan error, 1)
	go func() { _, err := other.CreateSnapshot(ctx, main.ID, main.HeadLSN); publicationDone <- err }()
	select {
	case err := <-publicationDone:
		t.Fatalf("publication passed active GC lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(store.resume)
	require.NoError(t, <-gcDone)
	require.NoError(t, <-publicationDone)
	state, err := otherMat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	require.EqualValues(t, 1, state["d"]["v"])
}
