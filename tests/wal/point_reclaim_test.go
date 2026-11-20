package wal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSource_RetainedRowDeletedInSnapshot(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "retained-delete")
	_, err := h.writer.Put(ctx, "items", bson.M{"_id": "q"})
	require.NoError(t, err)
	at, deleted, err := h.writer.Delete(ctx, "items", "q")
	require.NoError(t, err)
	require.True(t, deleted)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "items", at))
	removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": at})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed.DeletedCount)

	branch := h.reload()
	state, err := h.mat.MaterializeCollection(branch, "items")
	require.NoError(t, err)
	require.NotContains(t, state, "q")
	doc, err := h.mat.MaterializeDocument(branch, "items", "q")
	require.NoError(t, err)
	require.Nil(t, doc)
	next, err := h.writer.Put(ctx, "items", bson.M{"_id": "q", "v": "new"})
	require.NoError(t, err)
	entry, err := h.wal.GetEntry(h.project, next)
	require.NoError(t, err)
	require.Empty(t, entry.PreImage)
}

func TestSource_RetainedRowReadErrors(t *testing.T) {
	for _, fault := range []string{"floor", "chunk", "legacy row"} {
		t.Run(fault, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "retained-error")
			at, err := h.writer.Put(ctx, "items", bson.M{"_id": "q"})
			require.NoError(t, err)
			_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
			require.NoError(t, err)
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "items", at))

			chunkError := errors.New("failed to read test chunk")
			switch fault {
			case "floor":
				_, err = h.db.Collection("wal_reclaim_floors").UpdateOne(ctx,
					bson.M{"_id.branch_id": h.main.ID, "_id.collection": "items"},
					bson.M{"$set": bson.M{"lsn": "invalid"}})
			case "chunk":
				_, err = snapshot.NewServiceWithStore(h.db, h.branches, h.mat, certificationStore{
					ChunkStore: snapshot.NewMongoChunkStore(h.db),
					read:       func(context.Context, string) ([]byte, error) { return nil, chunkError },
				})
			case "legacy row":
				_, err = h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": at},
					bson.M{"$set": bson.M{"operation": wal.LegacyOpInsert}})
			}
			require.NoError(t, err)
			_, err = h.writer.Put(ctx, "items", bson.M{"_id": "q", "v": "new"})
			switch fault {
			case "floor":
				require.ErrorContains(t, err, "failed to read reclaim floor")
			case "chunk":
				require.ErrorIs(t, err, chunkError)
			case "legacy row":
				require.ErrorContains(t, err, "legacy schema-v1")
			}
			require.Equal(t, at, h.reload().HeadLSN)
		})
	}
}

func TestSource_RetainedRowWithoutCoverage(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "retained-without-coverage")
	a, b := bson.M{"_id": "q", "v": "A"}, bson.M{"_id": "q", "v": "B"}
	_, err := h.writer.Put(ctx, "docs", a)
	require.NoError(t, err)
	snap, err := h.writer.Put(ctx, "docs", b)
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, snap)
	require.NoError(t, err)
	head, err := h.writer.Put(ctx, "docs", bson.M{"_id": "other"})
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", head))
	removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": snap})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed.DeletedCount)

	branch := h.reload()
	doc, err := h.mat.MaterializeDocumentAtLSN(branch, "docs", "q", snap)
	require.NoError(t, err)
	require.Equal(t, b, doc)
	doc, err = h.mat.MaterializeDocument(branch, "docs", "q")
	require.NoError(t, err)
	require.Equal(t, a, doc, "the visible range above the snapshot reaches the floor")
	doc, err = h.matFull.MaterializeDocumentAtLSN(branch, "docs", "q", snap)
	require.NoError(t, err)
	require.Equal(t, a, doc, "no snapshot provider")

	// A caller can still hold branch metadata loaded before the floor was forgotten.
	require.NoError(t, h.wal.ForgetReclaimFloors(ctx, branch.ID))
	doc, err = h.mat.MaterializeDocumentAtLSN(branch, "docs", "q", snap)
	require.NoError(t, err)
	require.Equal(t, a, doc, "a stale gate alone cannot select the snapshot")
}

func TestSource_OlderGCLeavesReclaimGateBehind(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "old-gc")
	start := h.put("seed", 1)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, start)
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", start))
	_, err = h.wal.DeleteDataEntriesUpTo(h.main.ID, "docs", start)
	require.NoError(t, err)

	a, b := bson.M{"_id": "q", "v": "A"}, bson.M{"_id": "q", "v": "B"}
	first, err := h.writer.Put(ctx, "docs", a)
	require.NoError(t, err)
	require.Greater(t, first, start)
	last, err := h.writer.Put(ctx, "docs", b)
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
	require.NoError(t, err)
	// v2.8.1 updates only the floor. Interrupt its deletion after B.
	floors := h.db.Collection("wal_reclaim_floors")
	updated, err := floors.UpdateOne(ctx, bson.M{"_id.branch_id": h.main.ID, "_id.collection": "docs"},
		bson.M{"$max": bson.M{"lsn": last}})
	require.NoError(t, err)
	require.EqualValues(t, 1, updated.ModifiedCount)
	removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": last})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed.DeletedCount)

	branch := h.reload()
	doc, err := h.mat.MaterializeDocument(branch, "docs", "q")
	require.NoError(t, err)
	require.Equal(t, a, doc)
	state, err := h.mat.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	require.Equal(t, b, state["q"])
	next, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "v": "D"})
	require.NoError(t, err)
	entry, err := h.wal.GetEntry(h.project, next)
	require.NoError(t, err)
	var pre bson.M
	require.NoError(t, wal.UnmarshalDocument(entry.PreImage, &pre))
	require.Equal(t, a, pre)
}
