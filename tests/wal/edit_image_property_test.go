package wal_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Compare every historical position, including positions between the branch's
// own writes and positions below its fork. The decoded point reader documents
// a collection replay fallback for histories it cannot resolve by key.
func TestSource_EditImageReadParity(t *testing.T) {
	for _, seed := range []int64{31, 97} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			checkEditImageHistory(t, seed)
		})
	}
}

const editCollection = "stock"

func checkEditImageHistory(t *testing.T, seed int64) {
	t.Helper()
	h := newHistoryFixture(t, "image-parity")
	ctx := t.Context()
	rng := rand.New(rand.NewSource(seed))
	keys := []string{"chair", "lamp", "desk"}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	a, b, c := keys[0], keys[1], keys[2]
	appendRow := func(branch *wal.Branch, key string, op wal.OperationType, raw bson.Raw, unverified bool) int64 {
		t.Helper()
		row := &wal.Entry{ProjectID: h.project, BranchID: branch.ID, Collection: editCollection, DocumentID: key, Operation: op}
		if op == wal.OpPut {
			row.PostImage = raw
		} else {
			row.PreImage = raw
		}
		if unverified {
			row.Metadata = map[string]any{"capture_images": "attached_unverified"}
		}
		at, err := h.wal.Append(row)
		require.NoError(t, err)
		require.NoError(t, h.branches.UpdateBranchHead(branch.ID, at))
		branch.HeadLSN = at
		return at
	}
	fork := func(parent *wal.Branch, name string) *wal.Branch {
		t.Helper()
		child, err := h.branches.CreateBranch(h.project, name, parent.ID)
		require.NoError(t, err)
		return child
	}
	// All three historical manifest formats below hold just one document.
	// Sorted frames come from decoded replay; the ordered frame deliberately
	// retains a duplicated name, as an existing 7e3c155 chunk can.
	manifest := func(at int64, version string, doc bson.D) {
		t.Helper()
		id := insertReleasedSnapshot(t, h.db, h.main, editCollection, at, a, doc)
		update := bson.M{"$set": bson.M{"exact_old_binary_keys": true}}
		switch version {
		case "2.1.5":
			update = bson.M{"$unset": bson.M{"visibility_rules": ""}}
		case "ordered":
			update = bson.M{"$set": bson.M{"exact_old_binary_keys": true, "ordered_images": true}}
		}
		_, err := h.db.Collection("wal_snapshots").UpdateByID(ctx, id, update)
		require.NoError(t, err)
	}
	sortedFrame := func(at int64) bson.D {
		t.Helper()
		state, err := h.matFull.MaterializeCollectionAtLSN(h.main, editCollection, at)
		require.NoError(t, err)
		require.Len(t, state, 1)
		raw, err := wal.MarshalDocument(state[a])
		require.NoError(t, err)
		var fields bson.D
		require.NoError(t, wal.UnmarshalDocument(raw, &fields))
		return fields
	}
	image := func(key, shape string) bson.D {
		n := rng.Int31()
		return duplicateMergeDocument(key, shape, n, n+1)
	}
	first := bson.D{{Key: "_id", Value: a}, {Key: "z", Value: rng.Int31()}, {Key: "a", Value: rng.Int31()}}
	original := mustMarshalBSON(first)
	start := appendRow(h.main, a, wal.OpPut, original, false)
	early := fork(h.main, "early")
	first[1], first[2] = first[2], first[1]
	reordered := mustMarshalBSON(first)
	missing := appendRow(h.main, a, wal.OpPut, reordered, false)
	attached := mustMarshalBSON(image(b, "array"))
	appendRow(h.main, b, wal.OpPut, attached, true)
	late := fork(h.main, "late")
	at := appendRow(h.main, b, wal.OpDelete, attached, false)
	manifest(at, "2.1.5", sortedFrame(at))
	at = appendRow(h.main, a, wal.OpPut, mustMarshalBSON(image(a, "top")), false)
	manifest(at, "2.8.0", sortedFrame(at))
	reclaimThrough := at
	dirty := image(a, "nested")
	at = appendRow(h.main, a, wal.OpPut, mustMarshalBSON(dirty), false)
	manifest(at, "ordered", dirty)
	at = appendRow(h.main, c, wal.OpPut, mustMarshalBSON(image(c, "array")), false)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	frames, frameLSN, ok, ordered, err := h.snapshots.FindUsableFrames(h.main, editCollection, at, at, nil)
	require.NoError(t, err)
	require.True(t, ok && ordered)
	require.Equal(t, at, frameLSN)
	for _, raw := range frames {
		requireDistinctImageFields(t, raw)
	}

	// A historical image may identify its row only through the image _id.
	at = appendRow(early, b, wal.OpPut, mustMarshalBSON(image(b, "nested")), false)
	_, err = h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": early.ID, "lsn": at},
		bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": "old-envelope"}})
	require.NoError(t, err)
	appendRow(early, b, wal.OpDelete, nil, false)
	appendRow(late, a, wal.OpPut, mustMarshalBSON(image(a, "array")), false)
	grandchild := fork(late, "grandchild")
	appendRow(late, c, wal.OpPut, mustMarshalBSON(image(c, "scope")), false)
	// Different seeds overwrite and delete different inherited keys.
	shapes := []string{"top", "nested", "array", "scope"}
	for range 6 {
		key := keys[rng.Intn(len(keys))]
		if rng.Intn(3) == 0 {
			appendRow(grandchild, key, wal.OpDelete, nil, false)
		} else {
			appendRow(grandchild, key, wal.OpPut, mustMarshalBSON(image(key, shapes[rng.Intn(len(shapes))])), rng.Intn(2) == 0)
		}
	}

	// The old envelope never names a document in the materialized state.
	readKeys := append(keys, "old-envelope")
	check := func(t *testing.T) {
		t.Helper()
		for _, branch := range []*wal.Branch{h.main, early, late, grandchild} {
			t.Run(branch.Name, func(t *testing.T) {
				t.Parallel()
				for pos := int64(0); pos <= branch.HeadLSN; pos++ {
					checkEditImagePosition(t, h, branch, pos, readKeys)
				}
			})
		}
	}
	t.Run("intact", check)
	// Simulate an interrupted old GC: an intervening same-value reorder is
	// gone but the older image remains eligible for the decoded point read.
	result, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": missing})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.DeletedCount)
	t.Run("unrecorded reclamation", check)
	for _, tc := range []struct {
		pos  int64
		want bson.Raw
	}{{start, original}, {early.CreatedLSN, original}, {missing, reordered}} {
		// The surviving write establishes order at its own position and
		// through the retained fork control immediately after it. Across
		// the missing row only the decoded (name-sorted) value is known.
		got, err := h.mat.EditDocumentAtLSN(h.main, editCollection, a, tc.pos)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
		all, err := h.mat.EditCollectionAtLSN(h.main, editCollection, tc.pos)
		require.NoError(t, err)
		require.Equal(t, tc.want, all[a])
	}
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, editCollection, reclaimThrough))
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, editCollection, reclaimThrough)
	require.NoError(t, err)
	require.Positive(t, removed)
	t.Run("recorded reclamation", check)
}

// The point reader consults a key's retained rows before an intact
// descendant's snapshot. The collection reader starts at that snapshot.
// Partial reclamation without a floor can give these reads different values.
func TestSource_EditPointAcrossReclaimedParent(t *testing.T) {
	const key = "item"
	ctx := t.Context()
	h := newHistoryFixture(t, "edit-reclaimed-parent")
	version := func(v string) bson.M { return bson.M{"_id": key, "version": v} }
	_, err := h.writer.Put(ctx, editCollection, version("A"))
	require.NoError(t, err)
	missing, err := h.writer.Put(ctx, editCollection, version("B"))
	require.NoError(t, err)
	child, err := h.branches.CreateBranch(h.project, "child", h.main.ID)
	require.NoError(t, err)
	writer := walwriter.New(h.wal, h.branches, h.mat, child)
	at, err := writer.Put(ctx, editCollection, bson.M{"_id": "other"})
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, child.ID, at)
	require.NoError(t, err)

	later, err := h.writer.Put(ctx, editCollection, version("C"))
	require.NoError(t, err)
	manifest := insertReleasedSnapshot(t, h.db, h.main, editCollection, later, key,
		bson.D{{Key: "_id", Value: key}, {Key: "version", Value: "C"}})
	_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, manifest, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)
	removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": missing})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed.DeletedCount)

	point, err := h.mat.MaterializeDocumentsAtLSN(child, editCollection, []string{key}, at)
	require.NoError(t, err)
	require.Equal(t, version("A"), point[key])
	full, err := h.mat.MaterializeCollectionAtLSN(child, editCollection, at)
	require.NoError(t, err)
	require.Equal(t, version("B"), full[key])
	image, err := h.mat.EditDocumentAtLSN(child, editCollection, key, at)
	require.NoError(t, err)
	require.Equal(t, point[key], decodeEditImage(t, image))
	images, err := h.mat.EditCollectionAtLSN(child, editCollection, at)
	require.NoError(t, err)
	require.Equal(t, full[key], decodeEditImage(t, images[key]))
}

func checkEditImagePosition(t *testing.T, h *historyFixture, branch *wal.Branch, pos int64, keys []string) {
	t.Helper()
	label := fmt.Sprintf("%s at %d", branch.Name, pos)
	full, fullErr := h.mat.MaterializeCollectionAtLSN(branch, editCollection, pos)
	images, err := h.mat.EditCollectionAtLSN(branch, editCollection, pos)
	require.Equal(t, fullErr == nil, err == nil, "%s collection: decoded %v, ordered %v", label, fullErr, err)
	if fullErr == nil {
		require.Len(t, images, len(full), label)
		for key, doc := range full {
			require.Equal(t, doc, decodeEditImage(t, images[key]), "%s collection key %q", label, key)
		}
	}
	for _, key := range keys {
		docs, err := h.mat.MaterializeDocumentsAtLSN(branch, editCollection, []string{key}, pos)
		if err != nil {
			docs, err = full, fullErr
		}
		image, imageErr := h.mat.EditDocumentAtLSN(branch, editCollection, key, pos)
		require.Equal(t, err == nil, imageErr == nil, "%s key %q: decoded %v, ordered %v", label, key, err, imageErr)
		if err == nil {
			require.Equal(t, docs[key], decodeEditImage(t, image), "%s key %q", label, key)
		}
	}
}

func decodeEditImage(t *testing.T, raw bson.Raw) bson.M {
	t.Helper()
	if raw == nil {
		return nil
	}
	requireDistinctImageFields(t, raw)
	var doc bson.M
	require.NoError(t, wal.UnmarshalDocument(raw, &doc))
	return doc
}

func requireDistinctImageFields(t *testing.T, raw bson.Raw) {
	t.Helper()
	fields, err := raw.Elements()
	require.NoError(t, err)
	seen := make(map[string]bool)
	for _, f := range fields {
		require.False(t, seen[f.Key()], "repeated field %q in %v", f.Key(), raw)
		seen[f.Key()] = true
		v := f.Value()
		if v.Type == bson.TypeEmbeddedDocument || v.Type == bson.TypeArray {
			requireDistinctImageFields(t, v.Value)
		}
	}
}
