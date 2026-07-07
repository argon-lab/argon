package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/gc"
	"github.com/argon-lab/argon/internal/pin"
	"github.com/argon-lab/argon/internal/sandbox"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestPin_CRUDAndValidation(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("pin-crud", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 5; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	// LSN 0 pins the current head.
	head, err := pins.Create("pin-crud", main.ID, "at-head", 0, "eval suite v1")
	require.NoError(t, err)
	assert.Equal(t, main.HeadLSN, head.LSN)
	assert.Equal(t, "main", head.BranchName)

	// Names are unique per project.
	_, err = pins.Create("pin-crud", main.ID, "at-head", 0, "")
	require.ErrorContains(t, err, "already exists")

	// Bounds: beyond the head is refused.
	_, err = pins.Create("pin-crud", main.ID, "future", main.HeadLSN+100, "")
	require.ErrorContains(t, err, "outside branch range")

	// Wrong project is refused.
	_, err = pins.Create("other-project", main.ID, "cross", 0, "")
	require.ErrorContains(t, err, "does not belong")

	mid, err := pins.Create("pin-crud", main.ID, "mid", main.HeadLSN-2, "")
	require.NoError(t, err)

	list, err := pins.List("pin-crud")
	require.NoError(t, err)
	require.Len(t, list, 2)

	got, err := pins.Get("pin-crud", "mid")
	require.NoError(t, err)
	assert.Equal(t, mid.LSN, got.LSN)

	require.NoError(t, pins.Delete("pin-crud", "mid"))
	_, err = pins.Get("pin-crud", "mid")
	require.ErrorContains(t, err, "not found")
	require.ErrorContains(t, pins.Delete("pin-crud", "mid"), "not found")
}

func TestPin_BranchFromPinMaterializesPinnedState(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("pin-branch", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 4; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i), "v": int32(1)})
		require.NoError(t, err)
	}
	p, err := pins.Create("pin-branch", main.ID, "dataset-v1", 0, "")
	require.NoError(t, err)

	// The branch moves on: overwrites and new documents.
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "d0", "v": int32(2)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "d9", "v": int32(1)})
	require.NoError(t, err)

	run, err := f.restore.CreateBranchFromPin("pin-branch", p.BranchID, "eval-run-1", p.LSN)
	require.NoError(t, err)
	state, err := f.mat.MaterializeCollection(run, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 4, "pinned state has the original four documents")
	assert.EqualValues(t, 1, state["d0"]["v"], "pinned state predates the overwrite")
	assert.NotContains(t, state, "d9")
}

func TestPin_SurvivesReset(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("pin-reset", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 3; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("keep%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	resetTarget := main.HeadLSN

	for i := 0; i < 3; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("extra%d", i)})
		require.NoError(t, err)
	}
	p, err := pins.Create("pin-reset", main.ID, "with-extras", 0, "")
	require.NoError(t, err)

	// Reset the branch to before the extras: a discarded range now covers
	// the pinned entries — but only for readers beyond the pin.
	_, err = f.restore.ResetBranchToLSN(main.ID, resetTarget)
	require.NoError(t, err)

	run, err := f.restore.CreateBranchFromPin("pin-reset", p.BranchID, "pinned-run", p.LSN)
	require.NoError(t, err)
	state, err := f.mat.MaterializeCollection(run, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 6, "the pin still sees the pre-reset state")
	assert.Contains(t, state, "extra0")

	// The branch itself reads post-reset state.
	main, _ = f.branches.GetBranchByID(main.ID)
	mainState, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.Len(t, mainState, 3)
}

func TestPin_GCKeepsPinnedHistory(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	gcService.SetPinLookup(pins.LSNsForBranch)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("pin-gc", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 5; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	pinLSN := main.HeadLSN
	p, err := pins.Create("pin-gc", main.ID, "dataset", pinLSN, "")
	require.NoError(t, err)

	for i := 5; i < 10; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	// The head snapshot covers everything for ordinary readers — but the
	// pin reads at its own LSN and no snapshot at or below it exists, so
	// nothing may be reclaimed.
	report, err := gcService.RunProject(ctx, "pin-gc", zeroRetention)
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved, "pinned history without pin-usable coverage survives")

	// A snapshot at the pin gives the pin its own floor: entries at or
	// below it are covered for every reader, the rest stay for the pin.
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, pinLSN)
	require.NoError(t, err)
	report, err = gcService.RunProject(ctx, "pin-gc", zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 5, report.EntriesRemoved, "entries at or below the pin's snapshot reclaimed")

	// The pinned state still materializes exactly after GC.
	run, err := f.restore.CreateBranchFromPin("pin-gc", p.BranchID, "post-gc-run", p.LSN)
	require.NoError(t, err)
	state, err := f.mat.MaterializeCollection(run, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 5)

	// Deleting the pin releases the hold: the head snapshot now covers all.
	require.NoError(t, f.branches.DeleteBranch("pin-gc", "post-gc-run"))
	require.NoError(t, pins.Delete("pin-gc", "dataset"))
	report, err = gcService.RunProject(ctx, "pin-gc", zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 5, report.EntriesRemoved, "unpinned history reclaimed up to the head snapshot")
}

func TestPin_DeleteGuardProtectsPinnedBranches(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	f.branches.SetDeleteGuard(pins.RequireNoPins)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("pin-guard", "main", "")
	require.NoError(t, err)
	other, err := f.branches.CreateBranch("pin-guard", "datasets", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, other)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "d"})
	require.NoError(t, err)

	_, err = pins.Create("pin-guard", other.ID, "hold", 0, "")
	require.NoError(t, err)

	err = f.branches.DeleteBranch("pin-guard", "datasets")
	require.ErrorContains(t, err, "pin")

	require.NoError(t, pins.Delete("pin-guard", "hold"))
	require.NoError(t, f.branches.DeleteBranch("pin-guard", "datasets"))
}

func TestPin_SandboxFromPin(t *testing.T) {
	f := newIngestFixture(t, "pin-sandbox")
	pins, err := pin.NewService(f.metaDB, f.branches)
	require.NoError(t, err)
	sandboxes := sandbox.NewService(f.branches, f.checkout)
	ctx := context.Background()

	// The fixture seeded one document; pin that state, then move on.
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	p, err := pins.Create("pin-sandbox", main.ID, "seed-state", 0, "")
	require.NoError(t, err)

	stop := f.startIngester(t)
	_, err = f.physical.Collection("users").InsertOne(ctx, bson.M{"_id": "later", "n": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "users", 2)
	stop()

	// Fork a sandbox from the pin: it materializes exactly the pinned state.
	_, err = f.restore.CreateBranchFromPin("pin-sandbox", p.BranchID, "", p.LSN)
	require.Error(t, err, "empty branch names are refused")
	branch, err := f.restore.CreateBranchFromPin("pin-sandbox", p.BranchID, "eval-1", p.LSN)
	require.NoError(t, err)
	info, err := sandboxes.Adopt(ctx, branch.ID, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = f.client.Database(info.PhysicalDB).Drop(context.Background())
	})
	assert.Equal(t, "main", info.ForkedFrom)
	assert.Equal(t, p.LSN, info.ForkLSN)

	docs := f.client.Database(info.PhysicalDB).Collection("users")
	count, err := docs.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count, "sandbox has the pinned seed state only")

	var seed bson.M
	require.NoError(t, docs.FindOne(ctx, bson.M{"_id": "seed"}).Decode(&seed))
}
