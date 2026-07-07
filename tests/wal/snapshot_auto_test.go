package wal_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSnapshot_AutoTrigger(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	// Synchronous mode with a tiny threshold so the test is deterministic:
	// check on every write, snapshot once the head is 10 LSNs past the
	// newest snapshot (or the fork point).
	f.snapshots.EnableAuto(snapshot.AutoConfig{
		Threshold:   10,
		CheckEvery:  1,
		Synchronous: true,
	})

	main, err := f.branches.CreateBranch("auto-snap", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	writer.SetAutoSnapshotter(f.snapshots)

	// Below the threshold: no snapshot yet.
	for i := 0; i < 5; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("a%d", i)})
		require.NoError(t, err)
	}
	snaps, err := f.snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	assert.Empty(t, snaps, "below threshold: no automatic snapshot")

	// Cross the threshold.
	for i := 5; i < 15; i++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("a%d", i)})
		require.NoError(t, err)
	}
	snaps, err = f.snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	require.NotEmpty(t, snaps, "threshold crossed: automatic snapshot taken")
	firstCount := len(snaps)

	// Immediately after, the head is close to the snapshot again — more
	// writes below the threshold must not snapshot again.
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "extra"})
	require.NoError(t, err)
	snaps, err = f.snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	assert.Len(t, snaps, firstCount, "no re-snapshot while within threshold of the last one")

	// State served through the auto-snapshot equals full replay.
	main, _ = f.branches.GetBranchByID(main.ID)
	f.requireSnapshotMatchesFullReplay(t, main, "auto-snapshot state")
}

func TestSnapshot_CleanupOnBranchDelete(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()

	// Wire the delete hook the way walcli does.
	f.branches.SetDeleteHook(func(branchID string) {
		_, _, err := f.snapshots.CleanupBranch(ctx, branchID)
		require.NoError(t, err)
	})

	main, err := f.branches.CreateBranch("gc-test", "main", "")
	require.NoError(t, err)
	mainWriter := walwriter.New(f.wal, f.branches, f.mat, main)

	// Shared content: main and the doomed branch snapshot identical
	// "shared" collections, so their chunks are deduplicated across both.
	docs := make([]bson.M, 50)
	for i := range docs {
		docs[i] = bson.M{"_id": fmt.Sprintf("s%02d", i), "payload": "shared"}
	}
	_, err = mainWriter.PutMany(ctx, "shared", docs)
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)

	// The doomed branch: inherits "shared" (same chunks) plus its own data.
	doomed, err := f.branches.CreateBranch("gc-test", "doomed", main.ID)
	require.NoError(t, err)
	doomedWriter := walwriter.New(f.wal, f.branches, f.mat, doomed)
	_, err = doomedWriter.Put(ctx, "own", bson.M{"_id": "only-here", "payload": "doomed"})
	require.NoError(t, err)
	doomed, _ = f.branches.GetBranchByID(doomed.ID)
	_, err = f.snapshots.CreateSnapshot(ctx, doomed.ID, doomed.HeadLSN)
	require.NoError(t, err)

	chunksBefore, err := db.Collection("wal_snapshot_chunks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)

	require.NoError(t, f.branches.DeleteBranch("gc-test", "doomed"))

	// Doomed manifests gone; shared chunks survive because main's
	// snapshot still references them; the branch-only chunk is gone.
	snaps, err := f.snapshots.ListSnapshots(ctx, doomed.ID)
	require.NoError(t, err)
	assert.Empty(t, snaps, "deleted branch keeps no manifests")

	chunksAfter, err := db.Collection("wal_snapshot_chunks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.Less(t, chunksAfter, chunksBefore, "orphaned chunks reclaimed")

	// Main still materializes through its snapshot untouched.
	main, _ = f.branches.GetBranchByID(main.ID)
	f.requireSnapshotMatchesFullReplay(t, main, "survivor after sibling GC")
	state, err := f.mat.MaterializeCollection(main, "shared")
	require.NoError(t, err)
	assert.Len(t, state, 50)
}
