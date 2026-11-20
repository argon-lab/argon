package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSource_GCReclaimGateLifecycle(t *testing.T) {
	const collection = "items.$stock"
	ctx := t.Context()
	h := newHistoryFixture(t, "gc-gate")
	at, err := h.writer.Put(ctx, collection, bson.M{"_id": "q"})
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	collector := gc.NewService(h.wal, h.branches, h.snapshots)
	report, err := collector.RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 1, report.EntriesRemoved)

	// A lower cutoff cannot move either number backwards.
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, collection, at-1))
	main := h.reload()
	require.Equal(t, at, main.ReclaimGate(collection))
	require.Zero(t, main.ReclaimGate("items"))
	require.Zero(t, main.ReclaimGate("$stock"))
	floor, err := h.wal.ReclaimFloor(main.ID, collection)
	require.NoError(t, err)
	require.Equal(t, at, floor)

	child, err := h.restore.CreateBranchAtLSN(h.project, main.ID, "child", at)
	require.NoError(t, err)
	require.Zero(t, child.ReclaimGate(collection))
	// The collection prefix and the dotted name have independent gates.
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, main.ID, "items", at-1))
	require.Equal(t, at-1, h.reload().ReclaimGate("items"))
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, child.ID, collection, at))
	h.branches.SetDeleteHook(func(id string) {
		_, _, _, err := collector.ReclaimDeletedBranch(ctx, id)
		require.NoError(t, err)
	})
	require.NoError(t, h.branches.DeleteBranch(h.project, child.Name))
	deleted, err := h.branches.GetBranchByIDAny(child.ID)
	require.NoError(t, err)
	require.Empty(t, deleted.ReclaimGates)
	floor, err = h.wal.ReclaimFloor(child.ID, collection)
	require.NoError(t, err)
	require.Zero(t, floor)
	require.Equal(t, at, h.reload().ReclaimGate(collection))

	// Both unique keys remain reserved by the tombstone. If it is physically
	// removed, recreating its ID/name from copied metadata must start clean.
	_, err = h.db.Collection("wal_branches").DeleteOne(ctx, bson.M{"_id": child.ID})
	require.NoError(t, err)
	replacement := *main
	replacement.ID, replacement.Name = child.ID, child.Name
	require.NoError(t, h.branches.CreateBranchWithData(&replacement))
	fresh, err := h.branches.GetBranchByID(child.ID)
	require.NoError(t, err)
	require.Zero(t, fresh.ReclaimGate(collection))
}
