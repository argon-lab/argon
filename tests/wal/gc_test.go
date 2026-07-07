package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/gc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// zeroRetention makes every existing entry immediately out-of-window, so
// tests exercise pure coverage logic.
var zeroRetention = gc.Config{RetentionWindow: 0}

func countBranchEntries(t *testing.T, f *snapshotFixture, branchID string) int {
	t.Helper()
	entries, err := f.wal.GetBranchEntries(branchID, "", 0, int64(1)<<62)
	require.NoError(t, err)
	return len(entries)
}

func TestGC_RequiresSnapshotCoverage(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("gc-cover", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 10; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}

	// No snapshot: nothing may be reclaimed, however old the entries are.
	before := countBranchEntries(t, f, main.ID)
	report, err := gcService.RunProject(ctx, "gc-cover", zeroRetention)
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved, "uncovered history is never deleted")
	assert.Equal(t, before, countBranchEntries(t, f, main.ID))

	// Snapshot, then GC: covered entries go, state is unchanged.
	main, _ = f.branches.GetBranchByID(main.ID)
	stateBefore, err := f.matFull.MaterializeBranch(main)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	report, err = gcService.RunProject(ctx, "gc-cover", zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 10, report.EntriesRemoved, "all covered data entries reclaimed")

	stateAfter, err := f.mat.MaterializeBranch(main)
	require.NoError(t, err)
	requireSameState(t, stateBefore, stateAfter, "state after GC")

	// Idempotent.
	report, err = gcService.RunProject(ctx, "gc-cover", zeroRetention)
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved)
}

func TestGC_RetentionWindowKeepsRecentHistory(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("gc-window", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 5; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	// Everything is younger than an hour: a one-hour window reclaims nothing.
	report, err := gcService.RunProject(ctx, "gc-window", gc.Config{RetentionWindow: time.Hour})
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved, "in-window history must survive even when covered")
}

func TestGC_LiveChildForkPinsHistory(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("gc-fork", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 8; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	// Fork first, snapshot afterwards: the snapshot is newer than the fork
	// point, so the child cannot use it — its history must stay pinned.
	child, err := f.branches.CreateBranch("gc-fork", "child", main.ID)
	require.NoError(t, err)
	for i := 8; i < 12; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	report, err := gcService.RunProject(ctx, "gc-fork", zeroRetention)
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved,
		"a live child without a usable snapshot at or below its fork pins the parent's history")

	childState, err := f.mat.MaterializeCollection(child, "docs")
	require.NoError(t, err)
	assert.Len(t, childState, 8, "child still materializes its fork state")

	// The child gains coverage only from a snapshot at or below its fork
	// point. Delete the child instead: the pin disappears and the parent
	// reclaims. (No delete hook is wired in this fixture, so deletion only
	// flips the flag — exactly what this test needs.)
	require.NoError(t, f.branches.DeleteBranch("gc-fork", "child"))

	report, err = gcService.RunProject(ctx, "gc-fork", zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 12, report.EntriesRemoved, "unpinned covered history reclaimed")

	mainState, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.Len(t, mainState, 12)
}

func TestGC_SnapshotBeforeForkAllowsReclaim(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("gc-fork2", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	for i := 0; i < 6; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}
	main, _ = f.branches.GetBranchByID(main.ID)

	// Snapshot first, fork afterwards: the child's fork point is at (or
	// above) the snapshot, so the child reads through the snapshot and the
	// raw entries below it are reclaimable.
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	child, err := f.branches.CreateBranch("gc-fork2", "child", main.ID)
	require.NoError(t, err)

	report, err := gcService.RunProject(ctx, "gc-fork2", zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 6, report.EntriesRemoved,
		"child forked at the snapshot reads through it; raw entries reclaimed")

	childState, err := f.mat.MaterializeCollection(child, "docs")
	require.NoError(t, err)
	assert.Len(t, childState, 6, "child materializes from the parent snapshot after GC")
}

func TestGC_DeletedBranchReclaimsEverything(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	ctx := context.Background()

	// Wire the hook the way walcli does.
	f.branches.SetDeleteHook(func(branchID string) {
		_, _, _, err := gcService.ReclaimDeletedBranch(ctx, branchID)
		require.NoError(t, err)
	})

	main, err := f.branches.CreateBranch("gc-del", "main", "")
	require.NoError(t, err)
	mainWriter := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = mainWriter.Put(ctx, "docs", bson.M{"_id": "keep"})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	doomed, err := f.branches.CreateBranch("gc-del", "doomed", main.ID)
	require.NoError(t, err)
	doomedWriter := walwriter.New(f.wal, f.branches, f.mat, doomed)
	for i := 0; i < 20; i++ {
		_, err := doomedWriter.Put(ctx, "scratch", bson.M{"_id": fmt.Sprintf("s%d", i)})
		require.NoError(t, err)
	}
	doomed, _ = f.branches.GetBranchByID(doomed.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, doomed.ID, doomed.HeadLSN)
	require.NoError(t, err)

	require.NoError(t, f.branches.DeleteBranch("gc-del", "doomed"))

	assert.Zero(t, countBranchEntries(t, f, doomed.ID), "deleted branch keeps no WAL entries")
	snaps, err := f.snapshots.ListSnapshots(ctx, doomed.ID)
	require.NoError(t, err)
	assert.Empty(t, snaps, "deleted branch keeps no snapshots")

	// The parent is untouched.
	state, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 1)
}
