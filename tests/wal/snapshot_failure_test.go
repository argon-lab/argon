package wal_test

import (
	"reflect"
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSourceSnapshotFailurePreservesHistory(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "snapshot-failure")
	at, err := h.writer.Put(ctx, "z", bson.M{"_id": "q"})
	require.NoError(t, err)
	snaps, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	require.Len(t, snaps[0].ChunkIDs, 1)
	first, err := h.writer.Put(ctx, "a", bson.M{"_id": "q", "rev": "A"})
	require.NoError(t, err)
	last, err := h.writer.Put(ctx, "a", bson.M{"_id": "q", "rev": "B"})
	require.NoError(t, err)

	store := snapshot.NewMongoChunkStore(h.db)
	chunk := snaps[0].ChunkIDs[0]
	data, err := store.Get(ctx, chunk)
	require.NoError(t, err)
	require.NoError(t, store.Delete(ctx, []string{chunk}))
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
	require.ErrorContains(t, err, chunk)
	restored, err := store.Put(ctx, data)
	require.NoError(t, err)
	require.Equal(t, chunk, restored)

	count, err := h.db.Collection("wal_snapshots").CountDocuments(ctx, bson.M{"branch_id": h.main.ID, "lsn": last})
	require.NoError(t, err)
	assert.Zero(t, count)
	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 1, report.EntriesRemoved, "only z has a completed snapshot")
	doc, err := h.mat.MaterializeDocumentAtLSN(h.reload(), "a", "q", first)
	require.NoError(t, err)
	require.Equal(t, "A", doc["rev"])
}

func TestSourceSnapshotFallsBackAfterChunkError(t *testing.T) {
	for _, mode := range []string{"same LSN", "earlier LSN"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "snapshot-chunk-fallback")
			sorted := bson.D{{Key: "_id", Value: "q"}, {Key: "a", Value: int32(1)}, {Key: "z", Value: int32(17)}}
			raw := mustMarshalBSON(bson.D{sorted[0], sorted[2], sorted[1]})
			first, err := h.wal.Append(&wal.Entry{ProjectID: h.project, BranchID: h.main.ID,
				Collection: "docs", DocumentID: "q", Operation: wal.OpPut, PostImage: raw})
			require.NoError(t, err)
			require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, first))
			id := insertReleasedSnapshot(t, h.db, h.main, "docs", first, "q", sorted)
			_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, id, bson.M{"$set": bson.M{"exact_old_binary_keys": true}})
			require.NoError(t, err)
			at := first
			if mode == "earlier LSN" {
				at, err = h.writer.Put(ctx, "other", bson.M{"_id": "anchor"})
				require.NoError(t, err)
			}
			snaps, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
			require.NoError(t, err)
			var ordered *snapshot.Snapshot
			for _, snap := range snaps {
				if snap.Collection == "docs" {
					ordered = snap
				}
			}
			require.NotNil(t, ordered)
			require.Len(t, ordered.ChunkIDs, 1)
			var released snapshot.Snapshot
			require.NoError(t, h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"_id": id}).Decode(&released))
			require.NotEqual(t, released.ChunkIDs, ordered.ChunkIDs)

			if mode == "same LSN" {
				report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
				require.NoError(t, err)
				require.EqualValues(t, 1, report.EntriesRemoved)
			} else {
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", first))
				n, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, "docs", first)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			}
			// The first chunk is readable. Both its extra key and its ordered
			// q must be discarded when the second chunk fails.
			store := snapshot.NewMongoChunkStore(h.db)
			frame := func(key string, doc any) []byte {
				return mustMarshalBSON(bson.D{{Key: "k", Value: key}, {Key: "d", Value: doc}})
			}
			prefix := append(frame("extra", bson.D{{Key: "_id", Value: "extra"}}), frame("q", raw)...)
			z, err := wal.NewCompressor(nil)
			require.NoError(t, err)
			data, err := z.Compress(prefix)
			require.NoError(t, err)
			require.NoError(t, z.Close())
			partial, err := store.Put(ctx, data)
			require.NoError(t, err)
			_, err = h.db.Collection("wal_snapshots").UpdateOne(ctx,
				bson.M{"branch_id": h.main.ID, "collection": "docs", "lsn": at, "ordered_images": true},
				bson.M{"$set": bson.M{"chunk_ids": bson.A{partial, ordered.ChunkIDs[0]}}})
			require.NoError(t, err)
			require.NoError(t, store.Delete(ctx, ordered.ChunkIDs))
			child, err := h.branches.CreateBranch(h.project, "child", h.main.ID)
			require.NoError(t, err)
			state, err := h.mat.MaterializeCollection(child, "docs")
			require.NoError(t, err)
			require.Len(t, state, 1)
			require.Equal(t, int32(17), state["q"]["z"])
			images, err := h.mat.EditCollectionAtLSN(child, "docs", child.HeadLSN)
			require.NoError(t, err)
			require.Len(t, images, 1)
			require.Equal(t, mustMarshalBSON(sorted), images["q"])
			missing, err := h.mat.MaterializeDocumentAtLSN(child, "docs", "extra", child.HeadLSN)
			require.NoError(t, err)
			require.Nil(t, missing)
			doc, err := h.mat.MaterializeDocumentAtLSN(child, "docs", "q", child.HeadLSN)
			require.NoError(t, err)
			require.Equal(t, state["q"], doc)
			writer := walwriter.New(h.wal, h.branches, h.mat, child)
			next, err := writer.Put(ctx, "docs", bson.M{"_id": "q", "z": 99})
			require.NoError(t, err)
			entry, err := h.wal.GetEntry(h.project, next)
			require.NoError(t, err)
			require.Equal(t, int32(17), entry.PreImage.Lookup("z").Int32())
			if mode == "earlier LSN" {
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", at))
				_, err := h.mat.MaterializeCollectionAtLSN(h.reload(), "docs", at)
				require.ErrorIs(t, err, wal.ErrHistoryReclaimed)

				// A certified pre-floor manifest can also cover unrecorded
				// deletes. Removing the floor must not make stale data readable.
				_, err = h.db.Collection("wal_snapshots").UpdateOne(ctx,
					bson.M{"branch_id": h.main.ID, "collection": "docs", "lsn": at},
					bson.M{"$unset": bson.M{"visibility_rules": "", "ordered_images": "", "exact_old_binary_keys": ""},
						"$set": bson.M{"old_key_check": "trusted", "old_keys_trusted": true}})
				require.NoError(t, err)
				_, err = h.db.Collection("wal_reclaim_floors").DeleteOne(ctx, bson.M{"_id.branch_id": h.main.ID, "_id.collection": "docs"})
				require.NoError(t, err)
				floor, err := h.wal.ReclaimFloor(h.main.ID, "docs")
				require.NoError(t, err)
				require.Zero(t, floor)
				_, err = h.mat.MaterializeCollectionAtLSN(h.main, "docs", at)
				require.ErrorContains(t, err, ordered.ChunkIDs[0])
			}
		})
	}
}

func TestSourceSnapshotSkipsOlderManifestDecode(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "snapshot-manifest-decode")
	manifests := h.db.Collection("wal_snapshots")
	var at int64
	for _, revision := range []int32{0, 1} {
		doc := bson.D{{Key: "_id", Value: "q"}, {Key: "rev", Value: revision}}
		lsn, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "rev": revision})
		require.NoError(t, err)
		id := insertReleasedSnapshot(t, h.db, h.main, "docs", lsn, "q", doc)
		fields := bson.M{"exact_old_binary_keys": true}
		if revision == 0 {
			fields["chunk_ids"] = "malformed"
		}
		_, err = manifests.UpdateByID(ctx, id, bson.M{"$set": fields})
		require.NoError(t, err)
		at = lsn
	}
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", at))
	n, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, "docs", at)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	registry := bson.NewRegistry()
	typ := reflect.TypeFor[snapshot.Snapshot]()
	decode, err := registry.LookupDecoder(typ)
	require.NoError(t, err)
	decodes := 0
	registry.RegisterTypeDecoder(typ, bsoncodec.ValueDecoderFunc(func(dc bsoncodec.DecodeContext, r bsonrw.ValueReader, v reflect.Value) error {
		decodes++
		return decode.DecodeValue(dc, r, v)
	}))
	db := h.db.Client().Database(h.db.Name(), options.Database().SetRegistry(registry))
	mat := materializer.NewService(h.wal, h.branches)
	reader, err := snapshot.NewService(db, h.branches, mat)
	require.NoError(t, err)
	main := h.reload()
	decodes = 0
	state, selected, ok, err := reader.FindUsable(main, "docs", 0, at, at)
	assert.Equal(t, 1, decodes, "only the newest manifest needs full decoding")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, at, selected)
	require.Equal(t, int32(1), state["q"]["rev"])

	next, err := walwriter.New(h.wal, h.branches, mat, main).Put(ctx, "docs", bson.M{"_id": "q", "rev": 9})
	require.NoError(t, err)
	entry, err := h.wal.GetEntry(h.project, next)
	require.NoError(t, err)
	require.Equal(t, int32(1), entry.PreImage.Lookup("rev").Int32())
}
