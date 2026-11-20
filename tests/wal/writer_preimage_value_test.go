package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestPutManyPreImageMatchesReplay(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "batch-preimage")
	first := bson.M{"_id": "q", "v": bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}, {Key: "z", Value: 3}}}
	_, err := h.writer.Put(ctx, "single", first)
	require.NoError(t, err)
	afterFirst, err := h.mat.MaterializeDocument(h.reload(), "single", "q")
	require.NoError(t, err)
	require.Equal(t, bson.M{"_id": "q", "v": bson.M{"a": int32(2), "z": int32(3)}}, afterFirst)

	lsns, err := h.writer.PutMany(ctx, "batch", []bson.M{first, {"_id": "q", "v": 4}})
	require.NoError(t, err)
	require.Len(t, lsns, 2)
	firstEntry, err := h.wal.GetEntry(h.project, lsns[0])
	require.NoError(t, err)
	input, err := wal.MarshalDocument(first)
	require.NoError(t, err)
	require.Equal(t, input, firstEntry.PostImage, "the first post-image retains both z fields")

	t.Run("replacement pre-image", func(t *testing.T) {
		secondEntry, err := h.wal.GetEntry(h.project, lsns[1])
		require.NoError(t, err)
		want, err := wal.MarshalDocument(afterFirst)
		require.NoError(t, err)
		require.Equal(t, want, secondEntry.PreImage)
	})

	t.Run("undo replacement", func(t *testing.T) {
		branch := h.reload()
		undos := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
		plan, err := undos.BuildPlan(branch, lsns[1], lsns[1], "")
		require.NoError(t, err)
		restored, deleted, err := undos.Apply(ctx, branch, plan)
		require.NoError(t, err)
		require.Equal(t, 1, restored)
		require.Zero(t, deleted)
		afterUndo, err := h.mat.MaterializeDocument(h.reload(), "batch", "q")
		require.NoError(t, err)
		require.Equal(t, afterFirst, afterUndo)
	})
}

func TestPutManyPlainPreImage(t *testing.T) {
	h := newHistoryFixture(t, "batch-plain-preimage")
	id := bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}}
	first := bson.M{"_id": id, "v": bson.M{"x": 1, "y": 2}}
	lsns, err := h.writer.PutMany(t.Context(), "items", []bson.M{first, {"_id": id, "v": 3}})
	require.NoError(t, err)
	require.Len(t, lsns, 2)
	entry, err := h.wal.GetEntry(h.project, lsns[1])
	require.NoError(t, err)
	want, err := wal.MarshalDocument(first)
	require.NoError(t, err)
	require.Equal(t, want, entry.PreImage)
}

func TestSource_WriterUndoLegacyDocumentKey(t *testing.T) {
	t.Skip("The writer can miss a legacy document key when reading its pre-image: https://github.com/argon-lab/argon/issues/154")
	const collection = "items"
	compoundID := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	for _, tc := range []struct {
		name, storedKey string
		id              any
		inheritsA       bool
	}{
		// The v2.0.1 importer marshalled the document and its extended-JSON
		// index key independently, so a compound ID could have two orders.
		{"v2.0.1 compound ID", `{"a":{"$numberInt":"2"},"z":{"$numberInt":"1"}}`, compoundID, false},
		{"opaque envelope over missing parent", "opaque-envelope", "q", false},
		{"opaque envelope over parent A", "opaque-envelope", "q", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			h := newHistoryFixture(t, "legacy-preimage")
			branch := h.main
			var singularWant bson.M
			if tc.inheritsA {
				singularWant = bson.M{"_id": tc.id, "version": "A"}
				_, err := h.writer.Put(ctx, collection, singularWant)
				require.NoError(t, err)
			}
			if tc.storedKey == "opaque-envelope" {
				child, err := h.branches.CreateBranch(h.project, "child", branch.ID)
				require.NoError(t, err)
				branch = child
			}

			want := bson.M{"_id": tc.id, "version": "B"}
			lsn, err := h.wal.Append(&wal.Entry{
				ProjectID: h.project, BranchID: branch.ID, Collection: collection, Actor: "importer",
				Operation: wal.OpPut, DocumentID: tc.storedKey, PostImage: mustMarshalBSON(want),
			})
			require.NoError(t, err)
			// Modern append normalizes the index key. Restore the legacy row's
			// key and absence of a key version, leaving its image intact.
			updated, err := h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": branch.ID, "lsn": lsn},
				bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": tc.storedKey}})
			require.NoError(t, err)
			require.EqualValues(t, 1, updated.ModifiedCount)
			require.NoError(t, h.branches.UpdateBranchHead(branch.ID, lsn))

			sandbox, err := h.branches.CreateBranch(h.project, "sandbox", branch.ID)
			require.NoError(t, err)
			dropPhysical(t, h.db.Client(), sandbox.ID)
			checkouts := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
			info, err := checkouts.Checkout(ctx, sandbox.ID)
			require.NoError(t, err)
			count, err := h.db.Client().Database(info.PhysicalDB).Collection(collection).CountDocuments(ctx, want)
			require.NoError(t, err)
			require.EqualValues(t, 1, count, "checkout contains B")

			if tc.storedKey != "opaque-envelope" {
				branch, err = h.branches.CreateBranch(h.project, "child", branch.ID)
			} else {
				branch, err = h.branches.GetBranchByID(branch.ID)
			}
			require.NoError(t, err)
			singular, err := h.mat.MaterializeDocument(branch, collection, wal.DocumentIDString(tc.id))
			require.NoError(t, err)
			require.Equal(t, singularWant, singular)
			checkWriterOverwriteAndUndo(t, h, branch, collection, want)
		})
	}
}

func TestSource_WriterUndoSnapshotOverRetainedRow(t *testing.T) {
	const collection, id = "items", "q"
	ctx := t.Context()
	h := newHistoryFixture(t, "snapshot-preimage")
	a := bson.M{"_id": id, "version": "A"}
	b := bson.M{"_id": id, "version": "B"}
	first, err := h.writer.Put(ctx, collection, a)
	require.NoError(t, err)
	require.EqualValues(t, 2, first)
	last, err := h.writer.Put(ctx, collection, b)
	require.NoError(t, err)
	require.EqualValues(t, 3, last)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, collection, last))
	removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": last})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed.DeletedCount)

	branch := h.reload()
	singular, err := h.mat.MaterializeDocument(branch, collection, id)
	require.NoError(t, err)
	require.Equal(t, b, singular)
	state, err := h.mat.MaterializeCollection(branch, collection)
	require.NoError(t, err)
	require.Equal(t, b, state[id], "snapshot covers the missing revision")
	checkWriterOverwriteAndUndo(t, h, branch, collection, b)
}

func checkWriterOverwriteAndUndo(t *testing.T, h *historyFixture, branch *wal.Branch, collection string, want bson.M) {
	t.Helper()
	ctx := t.Context()
	writer := walwriter.New(h.wal, h.branches, h.mat, branch)
	lsn, err := writer.Put(ctx, collection, bson.M{"_id": want["_id"], "version": "D"})
	require.NoError(t, err)
	entry, err := h.wal.GetEntry(h.project, lsn)
	require.NoError(t, err)
	var pre bson.M
	if len(entry.PreImage) > 0 {
		require.NoError(t, wal.UnmarshalDocument(entry.PreImage, &pre))
		pre["_id"], err = wal.DocumentIDFromImage(entry.PreImage)
		require.NoError(t, err)
	}
	assert.Equal(t, want, pre, "overwrite pre-image")

	branch, err = h.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	undos := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
	plan, err := undos.BuildPlan(branch, lsn, lsn, "")
	require.NoError(t, err)
	restored, deleted, err := undos.Apply(ctx, branch, plan)
	require.NoError(t, err)
	assert.Equal(t, 1, restored, "documents restored")
	assert.Zero(t, deleted, "documents deleted")
	branch, err = h.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	state, err := h.mat.MaterializeCollection(branch, collection)
	require.NoError(t, err)
	assert.Equal(t, want, state[wal.DocumentIDString(want["_id"])], "document after undo")
}
