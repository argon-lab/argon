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
			if mode == "earlier LSN" {
				require.ErrorContains(t, err, ordered.ChunkIDs[0])
				require.Nil(t, state)
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", at))
				_, err = h.mat.MaterializeCollectionAtLSN(h.reload(), "docs", at)
				require.ErrorContains(t, err, ordered.ChunkIDs[0])

				// A pre-floor manifest with an unavailable chunk also fails.
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
				return
			}
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
		})
	}
}

func TestSourceSnapshotMissingChunkAfterLegacyGC(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "snapshot-legacy-chunk-loss")
	manifests := h.db.Collection("wal_snapshots")
	var historical snapshot.Snapshot
	for i, rev := range []string{"A", "B", "C"} {
		lsn, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "rev": rev})
		require.NoError(t, err)
		require.EqualValues(t, i+2, lsn)
		doc := bson.D{{Key: "_id", Value: "q"}, {Key: "rev", Value: rev}}
		id := insertReleasedSnapshot(t, h.db, h.main, "docs", lsn, "q", doc)
		if rev == "B" {
			// v2.8.0 marked its sorted snapshots as having exact keys.
			_, err = manifests.UpdateByID(ctx, id, bson.M{"$set": bson.M{"exact_old_binary_keys": true}})
			require.NoError(t, err)
			require.NoError(t, manifests.FindOne(ctx, bson.M{"_id": id}).Decode(&historical))
		} else {
			// The v2.1.5 manifests at 2 and 4 predate visibility rules.
			_, err = manifests.UpdateByID(ctx, id, bson.M{"$unset": bson.M{"visibility_rules": ""}})
			require.NoError(t, err)
		}
	}
	h.snapshots.CertifyOldSnapshots(ctx)
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, "docs", 4)
	require.NoError(t, err)
	require.EqualValues(t, 3, removed)
	floor, err := h.wal.ReclaimFloor(h.main.ID, "docs")
	require.NoError(t, err)
	require.Zero(t, floor)

	child, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "fork", historical.LSN)
	require.NoError(t, err)
	doc, err := h.mat.MaterializeDocument(child, "docs", "q")
	require.NoError(t, err)
	require.Equal(t, "B", doc["rev"])
	require.Len(t, historical.ChunkIDs, 1)
	require.NoError(t, snapshot.NewMongoChunkStore(h.db).Delete(ctx, historical.ChunkIDs))
	missing := historical.ChunkIDs[0]
	state, err := h.mat.MaterializeCollection(child, "docs")
	assert.ErrorContains(t, err, missing)
	assert.Nil(t, state)
	doc, err = h.mat.MaterializeDocument(child, "docs", "q")
	assert.ErrorContains(t, err, missing)
	assert.Nil(t, doc)
	frames, err := h.mat.EditCollectionAtLSN(child, "docs", historical.LSN)
	assert.ErrorContains(t, err, missing)
	assert.Nil(t, frames)

	writer := walwriter.New(h.wal, h.branches, h.mat, child)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "q", "rev": "D"})
	assert.ErrorContains(t, err, missing)
	writes, err := h.db.Collection("wal_log").CountDocuments(ctx, bson.M{"branch_id": child.ID, "collection": "docs"})
	require.NoError(t, err)
	assert.Zero(t, writes)
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

	// A second sorted copy cannot improve the first, whichever comes first.
	id := insertReleasedSnapshot(t, h.db, h.main, "docs", at, "q", bson.D{{Key: "_id", Value: "q"}, {Key: "rev", Value: int32(1)}})
	_, err = manifests.UpdateByID(ctx, id, bson.M{"$set": bson.M{"exact_old_binary_keys": true}})
	require.NoError(t, err)

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
	assert.Equal(t, 1, decodes, "only one manifest needs full decoding")
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

func TestSourceSnapshotManifestCopies(t *testing.T) {
	missingChunk := bson.M{"exact_old_binary_keys": false, "ordered_images": true, "chunk_ids": bson.A{"unavailable"}}
	for _, tc := range []struct {
		name    string
		copies  []bson.M
		wantErr string
	}{
		{"damaged sorted sibling", []bson.M{{}, {"chunk_ids": "bad"}}, ""},
		{"damaged first", []bson.M{{"chunk_ids": "bad"}, {}}, ""},
		{"damaged ordered sibling", []bson.M{{}, {"ordered_images": true, "chunk_ids": "bad"}}, ""},
		{"unavailable chunk during certification", []bson.M{missingChunk, {}}, ""},
		{"malformed order marker", []bson.M{{"ordered_images": "bad"}}, ""},
		{"array order marker", []bson.M{{"ordered_images": bson.A{true}}}, ""},
		{"no decodable sibling", []bson.M{{"chunk_ids": "bad"}}, "chunk_ids"},
		{"no certifiable sibling", []bson.M{missingChunk}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "snapshot-manifest-copies")
			manifests := h.db.Collection("wal_snapshots")
			addCopy := func(at int64, rev int32, fields bson.M) {
				doc := bson.D{{Key: "_id", Value: "q"}, {Key: "rev", Value: rev}}
				id := insertReleasedSnapshot(t, h.db, h.main, "docs", at, "q", doc)
				if _, ok := fields["exact_old_binary_keys"]; !ok {
					fields["exact_old_binary_keys"] = true
				}
				_, err := manifests.UpdateByID(ctx, id, bson.M{"$set": fields})
				require.NoError(t, err)
			}
			first, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "rev": int32(0)})
			require.NoError(t, err)
			addCopy(first, 0, bson.M{})
			want := bson.M{"_id": "q", "rev": int32(1)}
			at, err := h.writer.Put(ctx, "docs", want)
			require.NoError(t, err)
			require.EqualValues(t, 3, at)
			for _, fields := range tc.copies {
				addCopy(at, 1, fields)
			}
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", at))
			n, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, "docs", at)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)

			main := h.reload()
			state, err := h.mat.MaterializeCollectionAtLSN(main, "docs", at)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, want, state["q"])
			doc, err := h.mat.MaterializeDocumentAtLSN(main, "docs", "q", at)
			require.NoError(t, err)
			require.Equal(t, want, doc)
			selected, ok, err := h.snapshots.UsableLSN(main, "docs", 0, at, at)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, at, selected)
			if len(tc.copies) == 1 {
				// Neither a string nor an array containing true certifies order.
				_, visit, err := h.snapshots.OrderedSnapshotFrames(main, "docs", at, at, at)
				require.NoError(t, err)
				require.Nil(t, visit)
			}
			next, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "rev": int32(9)})
			require.NoError(t, err)
			require.EqualValues(t, 4, next)
			entry, err := h.wal.GetEntry(h.project, next)
			require.NoError(t, err)
			require.Equal(t, int32(1), entry.PreImage.Lookup("rev").Int32())
		})
	}
}
