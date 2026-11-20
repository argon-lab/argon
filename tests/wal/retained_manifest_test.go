package wal_test

import (
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSource_RetainedRowReleasedSnapshots(t *testing.T) {
	oid := primitive.NewObjectID()
	for _, tc := range []struct {
		name    string
		want    bson.M
		sibling bson.M
		frame   bson.D
	}{
		{"2.0.1 hex string", bson.M{"_id": oid, "v": "object"}, bson.M{"_id": oid.Hex(), "v": "string"}, bson.D{{Key: "_id", Value: oid.Hex()}, {Key: "v", Value: "string"}}},
		{"2.7.0 binary field", bson.M{"_id": "q", "v": primitive.Binary{Subtype: 2, Data: []byte{}}}, nil, bson.D{{Key: "_id", Value: "q"}, {Key: "v", Value: primitive.Binary{Subtype: 2, Data: make([]byte, 4)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "retained-release")
			at, err := h.writer.Put(ctx, "items", tc.want)
			require.NoError(t, err)
			if tc.sibling != nil {
				at, err = h.writer.Put(ctx, "items", tc.sibling)
				require.NoError(t, err)
			}
			id := insertReleasedSnapshot(t, h.db, h.main, "items", at, wal.LegacyDocumentIDString(tc.frame[0].Value), tc.frame)
			if tc.sibling != nil {
				_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, id, bson.M{"$unset": bson.M{"visibility_rules": ""}})
				require.NoError(t, err)
			}
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "items", at))
			branch := h.reload()
			got, err := h.mat.MaterializeDocument(branch, "items", wal.DocumentIDString(tc.want["_id"]))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			checkWriterOverwriteAndUndo(t, h, branch, "items", tc.want)
		})
	}
}

func TestSource_RetainedManifestExactNames(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "retained-case")
	manifests := h.db.Collection("wal_snapshots")
	require.NoError(t, manifests.Drop(ctx))
	require.NoError(t, h.db.CreateCollection(ctx, "wal_snapshots", options.CreateCollection().SetCollation(&options.Collation{Locale: "en", Strength: 2})))
	_, err := h.writer.Put(ctx, "Things", bson.M{"_id": "q", "v": "upper"})
	require.NoError(t, err)
	want := bson.M{"_id": "q", "v": "lower"}
	at, err := h.writer.Put(ctx, "things", want)
	require.NoError(t, err)
	// Earlier candidates at the same position match MongoDB's collation.
	for _, pair := range [][2]string{{h.main.ID, "Things"}, {strings.ToUpper(h.main.ID), "things"}, {h.main.ID, "things"}} {
		v := "foreign"
		if pair == [2]string{h.main.ID, "things"} {
			v = "lower"
		}
		id := insertReleasedSnapshot(t, h.db, h.main, pair[1], at, "q", bson.D{{Key: "_id", Value: "q"}, {Key: "v", Value: v}})
		_, err = manifests.UpdateByID(ctx, id, bson.M{"$set": bson.M{"branch_id": pair[0], "ordered_images": true, "exact_old_binary_keys": true}})
		require.NoError(t, err)
	}
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "things", at))
	branch := h.reload()
	doc, err := h.mat.MaterializeDocument(branch, "things", "q")
	require.NoError(t, err)
	require.Equal(t, want, doc)
	checkWriterOverwriteAndUndo(t, h, branch, "things", want)
}

func TestSource_RetainedRowIneligibleChunk(t *testing.T) {
	h := newHistoryFixture(t, "retained-missing")
	ctx := t.Context()
	first, err := h.writer.Put(ctx, "docs", bson.M{"_id": "q", "v": "A"})
	require.NoError(t, err)
	snaps, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, first)
	require.NoError(t, err)
	want := bson.M{"_id": "q", "v": "B"}
	at, err := h.writer.Put(ctx, "docs", want)
	require.NoError(t, err)
	last := h.put("other", 1)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", last))
	_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, snaps[0].ID, bson.M{"$set": bson.M{"chunk_ids": bson.A{"missing"}}})
	require.NoError(t, err)
	got, err := h.mat.MaterializeDocumentAtLSN(h.reload(), "docs", "q", at)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSource_RetainedSnapshotSiblings(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(map[bool]string{false: "old sibling", true: "ordered sibling"}[eligible], func(t *testing.T) {
			h := newHistoryFixture(t, "retained-siblings")
			ctx := t.Context()
			at, err := h.writer.Put(ctx, "items", bson.M{"_id": "q", "v": "row"})
			require.NoError(t, err)
			// Several copies at the same LSN may differ in readability and format.
			for _, flags := range []bson.M{
				{"ordered_images": true, "chunk_ids": bson.A{"missing"}},
				{"ordered_images": bson.A{true}},
				{"ordered_images": eligible},
			} {
				id := insertReleasedSnapshot(t, h.db, h.main, "items", at, "q", bson.D{{Key: "_id", Value: "q"}, {Key: "v", Value: "frame"}})
				flags["exact_old_binary_keys"] = true
				_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, id, bson.M{"$set": flags})
				require.NoError(t, err)
			}
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "items", at))
			doc, err := h.mat.MaterializeDocument(h.reload(), "items", "q")
			if !eligible {
				require.ErrorContains(t, err, "missing")
				require.Nil(t, doc)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "frame", doc["v"])
		})
	}
}

func TestSource_RetainedRowReleasedCompoundID(t *testing.T) {
	h := newHistoryFixture(t, "retained-compound")
	ctx := t.Context()
	id := bson.D{{Key: "z", Value: 2}, {Key: "a", Value: 1}}
	key := wal.DocumentIDString(id)
	at, err := h.wal.Append(&wal.Entry{ProjectID: h.project, BranchID: h.main.ID, Collection: "docs", DocumentID: key, Operation: wal.OpPut, PostImage: mustMarshalBSON(bson.D{{Key: "_id", Value: id}, {Key: "v", Value: "retained"}})})
	require.NoError(t, err)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	insertReleasedSnapshot(t, h.db, h.main, "docs", at, key, bson.D{{Key: "_id", Value: bson.D{id[1], id[0]}}, {Key: "v", Value: "retained"}})
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", at))
	branch := h.reload()
	want, err := h.matFull.MaterializeDocument(branch, "docs", key)
	require.NoError(t, err)
	require.NotNil(t, want)
	got, err := h.mat.MaterializeDocument(branch, "docs", key)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSource_RetainedRowGapAboveMissingChunk(t *testing.T) {
	h := newHistoryFixture(t, "retained-gap")
	ctx := t.Context()
	want := bson.M{"_id": "q", "v": "A"}
	_, err := h.writer.Put(ctx, "docs", want)
	require.NoError(t, err)
	snap := h.put("other", 1)
	manifests, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, snap)
	require.NoError(t, err)
	head := h.put("later", 1)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, "docs", head))
	_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, manifests[0].ID, bson.M{"$set": bson.M{"chunk_ids": bson.A{"missing"}}})
	require.NoError(t, err)
	doc, err := h.mat.MaterializeDocument(h.reload(), "docs", "q")
	require.NoError(t, err)
	require.Equal(t, want, doc)
}
