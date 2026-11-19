package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSource_ReclaimedSnapshotAncestor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		exactAncestor bool
	}{{"uncovered", false}, {"exact-parent-snapshot", true}, {"discarded-after-exact-snapshot", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHistoryFixture(t, "ancestor-upgrade")
			ctx := t.Context()
			const collection = "inventory"
			ordinary := oldBinaryDocument("book", "visible at fork")
			fork := insertOldBinaryEntry(t, h.db, h.main, collection, "book", wal.OpPut, ordinary)
			require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, fork))
			if tc.exactAncestor {
				snaps, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, fork)
				require.NoError(t, err)
				require.Len(t, snaps, 1)
				require.True(t, snaps[0].ExactOldBinaryKeys)
			}
			floor, removedRows := fork, 1
			if tc.name == "discarded-after-exact-snapshot" {
				// The floor extends beyond the exact snapshot, but the
				// intervening entry is invisible from the fork point.
				floor = insertOldBinaryEntry(t, h.db, h.main, collection, "discarded", wal.OpPut, oldBinaryDocument("discarded", "removed"))
				require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, floor))
				h.reset(fork)
				fork = insertOldBinaryEntry(t, h.db, h.main, "notes", "marker", wal.OpPut, oldBinaryDocument("marker", "after reset"))
				require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, fork))
				removedRows++
			}
			child, err := h.branches.CreateBranch(h.project, "child", h.main.ID)
			require.NoError(t, err)
			require.Equal(t, fork, child.BaseLSN)
			at := insertOldBinaryEntry(t, h.db, child, "notes", "note", wal.OpPut, oldBinaryDocument("note", "after fork"))
			require.NoError(t, h.branches.UpdateBranchHead(child.ID, at))
			child, err = h.branches.GetBranchByID(child.ID)
			require.NoError(t, err)
			manifest := insertReleasedSnapshot(t, h.db, child, collection, at, "book", ordinary)

			// Only the parent's later WAL identifies the key type. Its row
			// must never appear in the child's historical state.
			id := bson.D{{Key: "part", Value: primitive.Binary{Subtype: 2, Data: []byte{9, 8, 7}}}}
			later := insertOldBinaryEntry(t, h.db, h.main, collection, wal.DocumentIDString(id), wal.OpPut, oldBinaryDocument(id, "after fork"))
			require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, later))
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, collection, floor))
			removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, collection, floor)
			require.NoError(t, err)
			require.EqualValues(t, removedRows, removed)

			upgraded := newSnapshotFixture(t, h.db)
			var rejected snapshot.Snapshot
			require.NoError(t, h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"_id": manifest}).Decode(&rejected))
			require.NotNil(t, rejected.OldKeysTrusted)
			require.False(t, *rejected.OldKeysTrusted)
			require.Equal(t, "reclaimed", rejected.OldKeyCheck)
			state, err := upgraded.mat.MaterializeCollectionAtLSN(child, collection, at)
			one, oneErr := upgraded.mat.MaterializeDocumentAtLSN(child, collection, "book", at)
			batch, batchErr := upgraded.mat.MaterializeDocumentsAtLSN(child, collection, []string{"book", "absent"}, at)
			readable := upgraded.mat.ValidateReadable(child, at)
			if tc.exactAncestor {
				require.NoError(t, err)
				require.Equal(t, []string{"book"}, idsOf(state))
				require.Equal(t, "visible at fork", state["book"]["value"])
				require.NoError(t, oneErr)
				require.Equal(t, state["book"], one)
				require.NoError(t, batchErr)
				require.Equal(t, state, batch)
				require.NoError(t, readable)
			} else {
				requireUnknownPrefix(t, err, h.main, collection, fork)
				require.Nil(t, state)
				requireUnknownPrefix(t, oneErr, h.main, collection, fork)
				require.Nil(t, one)
				requireUnknownPrefix(t, batchErr, h.main, collection, fork)
				require.Nil(t, batch)
				requireUnknownPrefix(t, readable, h.main, collection, fork)
			}
		})
	}
}

func TestSource_ReclaimedSnapshotCheckoutEvidence(t *testing.T) {
	h := newHistoryFixture(t, "checkout-upgrade")
	ctx := t.Context()
	const collection = "inventory"
	row := oldBinaryDocument("book", "earlier")
	at := insertOldBinaryEntry(t, h.db, h.main, collection, "book", wal.OpPut, row)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	client := h.db.Client()
	dropPhysical(t, client, h.main.ID)
	info, err := checkout.NewService(client, h.db, h.branches, h.mat).Checkout(ctx, h.main.ID)
	require.NoError(t, err)
	live := h.reload()
	require.True(t, live.IsLive())
	// This native write has not been captured. The checkout is the only
	// record of subtype 2, nested in an _id with a nonempty payload.
	id := bson.D{{Key: "owner", Value: "shop"}, {Key: "tag", Value: primitive.Binary{Subtype: 2, Data: []byte{1, 2, 3}}}}
	_, err = client.Database(info.PhysicalDB).Collection(collection).InsertOne(ctx, oldBinaryDocument(id, "live"))
	require.NoError(t, err)
	manifest := insertReleasedSnapshot(t, h.db, live, collection, at, "book", row)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, live.ID, collection, at))
	removed, err := h.wal.DeleteDataEntriesUpTo(live.ID, collection, at)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)

	upgraded := newSnapshotFixture(t, h.db)
	var rejected snapshot.Snapshot
	require.NoError(t, h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"_id": manifest}).Decode(&rejected))
	require.Equal(t, "reclaimed", rejected.OldKeyCheck)
	state, err := upgraded.mat.MaterializeCollectionAtLSN(live, collection, at)
	requireUnknownPrefix(t, err, live, collection, at)
	require.Nil(t, state)
}
