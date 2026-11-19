package wal_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Compare every historical position, including positions between the branch's
// own writes and positions below its fork, against decoded collection replay.
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
	var images []editImageRecord
	parents := map[string]*wal.Branch{h.main.ID: h.main}
	appendRow := func(branch *wal.Branch, key string, op wal.OperationType, pre, post bson.Raw, marker string) int64 {
		t.Helper()
		row := &wal.Entry{ProjectID: h.project, BranchID: branch.ID, Collection: editCollection, DocumentID: key,
			Operation: op, PreImage: pre, PostImage: post}
		if marker != "" {
			row.Metadata = map[string]any{"capture_images": marker}
		}
		at, err := h.wal.Append(row)
		require.NoError(t, err)
		require.NoError(t, h.branches.UpdateBranchHead(branch.ID, at))
		branch.HeadLSN = at
		images = append(images, editImageRecord{branch: branch.ID, key: key, at: at, raw: post, untrusted: marker == "attached_unverified"})
		return at
	}
	fork := func(parent *wal.Branch, name string) *wal.Branch {
		t.Helper()
		child, err := h.branches.CreateBranch(h.project, name, parent.ID)
		require.NoError(t, err)
		parents[child.ID] = child
		return child
	}
	// All three historical manifest formats below hold just one document.
	// Sorted frames come from decoded replay; the ordered frame deliberately
	// retains a duplicated name, as an existing 7e3c155 chunk can.
	manifest := func(at int64, version string, doc bson.D) {
		t.Helper()
		images = append(images, editImageRecord{branch: h.main.ID, key: a, at: at, raw: mustMarshalBSON(doc), frame: true, ordered: version == "ordered"})
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
	cleanImage := func(key string) bson.Raw {
		return mustMarshalBSON(duplicateMergeDocument(key, "nested", rng.Int31()))
	}
	first := bson.D{{Key: "_id", Value: a}, {Key: "z", Value: rng.Int31()}, {Key: "a", Value: rng.Int31()}}
	original := mustMarshalBSON(first)
	appendRow(h.main, a, wal.OpPut, nil, original, "current")
	early := fork(h.main, "early")
	first[1], first[2] = first[2], first[1]
	reordered := mustMarshalBSON(first)
	missing := appendRow(h.main, a, wal.OpPut, original, reordered, "")
	attached := mustMarshalBSON(image(b, "array"))
	appendRow(h.main, b, wal.OpPut, nil, attached, "attached_unverified")
	late := fork(h.main, "late")
	at := appendRow(h.main, b, wal.OpDelete, attached, nil, "current")
	manifest(at, "2.1.5", sortedFrame(at))
	dirtyTop := mustMarshalBSON(append(image(a, "top"), bson.E{Key: "_id", Value: c}))
	at = appendRow(h.main, a, wal.OpPut, reordered, dirtyTop, "current")
	manifest(at, "2.8.0", sortedFrame(at))
	reclaimThrough := at
	dirty := image(a, "nested")
	at = appendRow(h.main, a, wal.OpPut, dirtyTop, mustMarshalBSON(dirty), "")
	manifest(at, "ordered", dirty)
	at = appendRow(h.main, c, wal.OpPut, nil, cleanImage(c), "current")
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	frameLSN, walkFrames, err := h.snapshots.OrderedSnapshotFrames(h.main, editCollection, at, at, at)
	require.NoError(t, err)
	require.NotNil(t, walkFrames)
	require.Equal(t, at, frameLSN)
	require.NoError(t, walkFrames(func(key string, raw bson.Raw) error {
		requireDistinctImageFields(t, raw)
		images = append(images, editImageRecord{branch: h.main.ID, key: key, at: at, raw: bytes.Clone(raw), frame: true, ordered: true})
		return nil
	}))

	// A historical image may identify its row only through the image _id.
	at = appendRow(early, b, wal.OpPut, nil, cleanImage(b), "")
	_, err = h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": early.ID, "lsn": at},
		bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": "old-envelope"}})
	require.NoError(t, err)
	appendRow(early, b, wal.OpDelete, nil, nil, "")
	inherited := mustMarshalBSON(image(a, "array"))
	appendRow(late, a, wal.OpPut, reordered, inherited, "current")
	grandchild := fork(late, "grandchild")
	appendRow(late, c, wal.OpPut, nil, mustMarshalBSON(image(c, "scope")), "")
	appendRow(grandchild, a, wal.OpPut, inherited, cleanImage(a), "attached_unverified")
	// Different seeds overwrite and delete different inherited keys.
	shapes := []string{"top", "nested", "array", "scope"}
	markers := []string{"", "current", "attached_unverified"}
	for range 3 {
		key := keys[rng.Intn(len(keys))]
		if rng.Intn(3) == 0 {
			appendRow(grandchild, key, wal.OpDelete, nil, nil, "")
		} else {
			appendRow(grandchild, key, wal.OpPut, nil, mustMarshalBSON(image(key, shapes[rng.Intn(len(shapes))])), markers[rng.Intn(len(markers))])
		}
	}
	// Undo the marked write and include its compensation in collection replay.
	prior := cleanImage(b)
	appendRow(grandchild, b, wal.OpPut, nil, prior, "current")
	change := appendRow(grandchild, b, wal.OpPut, prior, mustMarshalBSON(image(b, "top")), "attached_unverified")
	grandchild, err = h.branches.GetBranchByID(grandchild.ID)
	require.NoError(t, err)
	undos := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
	plan, err := undos.BuildPlan(grandchild, change, change, "")
	require.NoError(t, err)
	restored, _, err := undos.Apply(ctx, grandchild, plan)
	require.NoError(t, err)
	require.Equal(t, 1, restored)
	grandchild, err = h.branches.GetBranchByID(grandchild.ID)
	require.NoError(t, err)
	compensation, err := h.wal.GetEntry(h.project, grandchild.HeadLSN)
	require.NoError(t, err)
	require.Equal(t, decodeEditImage(t, prior), decodeEditImage(t, compensation.PostImage))
	images = append(images, editImageRecord{branch: grandchild.ID, key: b, at: grandchild.HeadLSN, raw: compensation.PostImage})

	check := func(t *testing.T) {
		t.Helper()
		for _, branch := range []*wal.Branch{h.main, early, late, grandchild} {
			t.Run(branch.Name, func(t *testing.T) {
				t.Parallel()
				for pos := int64(0); pos <= branch.HeadLSN; pos++ {
					checkEditImagePosition(t, h, branch, pos, visibleEditImages(branch, pos, parents, images))
				}
			})
		}
	}
	t.Run("intact", check)
	// Simulate an interrupted old GC: an intervening same-value reorder is
	// gone while the older image remains in the WAL.
	result, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": missing})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.DeletedCount)
	images = slices.DeleteFunc(images, func(image editImageRecord) bool { return !image.frame && image.at == missing })
	t.Run("unrecorded reclamation", check)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, editCollection, reclaimThrough))
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, editCollection, reclaimThrough)
	require.NoError(t, err)
	require.Positive(t, removed)
	images = slices.DeleteFunc(images, func(image editImageRecord) bool {
		return !image.frame && image.branch == h.main.ID && image.at <= reclaimThrough
	})
	t.Run("recorded reclamation", check)
}

// A child snapshot retains B after the parent's row disappears. Both a fully
// typed child and one with a later untyped row exercise the batch shortcut,
// which resolves this key from the parent's surviving A.
func TestSource_EditPointAcrossReclaimedParent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("untyped=%t", legacy), func(t *testing.T) {
			const key = "item"
			ctx := t.Context()
			h := newHistoryFixture(t, "edit-reclaimed-parent")
			version := func(v string) bson.M { return bson.M{"_id": key, "version": v} }
			first, err := h.writer.Put(ctx, editCollection, version("A"))
			require.NoError(t, err)
			require.EqualValues(t, 2, first)
			missing, err := h.writer.Put(ctx, editCollection, version("B"))
			require.NoError(t, err)
			require.EqualValues(t, 3, missing)
			child, err := h.branches.CreateBranch(h.project, "child", h.main.ID)
			require.NoError(t, err)
			require.Equal(t, missing, child.BaseLSN)
			writer := walwriter.New(h.wal, h.branches, h.mat, child)
			at, err := writer.Put(ctx, editCollection, bson.M{"_id": "other"})
			require.NoError(t, err)
			require.EqualValues(t, 5, at)
			if legacy {
				at, err = writer.Put(ctx, editCollection, bson.M{"_id": "legacy"})
				require.NoError(t, err)
				_, err = h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": child.ID, "lsn": at},
					bson.M{"$unset": bson.M{"key_v": ""}})
				require.NoError(t, err)
			}
			_, err = h.snapshots.CreateSnapshot(ctx, child.ID, at)
			require.NoError(t, err)

			later, err := h.writer.Put(ctx, editCollection, version("C"))
			require.NoError(t, err)
			require.Equal(t, at+1, later)
			manifest := insertReleasedSnapshot(t, h.db, h.main, editCollection, later, key,
				bson.D{{Key: "_id", Value: key}, {Key: "version", Value: "C"}})
			_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, manifest, bson.M{"$unset": bson.M{"visibility_rules": ""}})
			require.NoError(t, err)
			removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": missing})
			require.NoError(t, err)
			require.EqualValues(t, 1, removed.DeletedCount)
			floor, err := h.wal.ReclaimFloor(h.main.ID, editCollection)
			require.NoError(t, err)
			require.Zero(t, floor)

			point, err := h.mat.MaterializeDocumentAtLSN(child, editCollection, key, at)
			require.NoError(t, err)
			require.Equal(t, version("B"), point)
			batch, err := h.mat.MaterializeDocumentsAtLSN(child, editCollection, []string{key}, at)
			require.NoError(t, err)
			require.Equal(t, version("A"), batch[key])
			full, err := h.mat.MaterializeCollectionAtLSN(child, editCollection, at)
			require.NoError(t, err)
			require.Equal(t, point, full[key])
			images, err := h.mat.EditCollectionAtLSN(child, editCollection, at)
			require.NoError(t, err)
			assert.Equal(t, point, decodeEditImage(t, images[key]))

			write, err := writer.Put(ctx, editCollection, version("D"))
			require.NoError(t, err)
			entry, err := h.wal.GetEntry(h.project, write)
			require.NoError(t, err)
			assert.Equal(t, point, decodeEditImage(t, entry.PreImage))
			child, err = h.branches.GetBranchByID(child.ID)
			require.NoError(t, err)
			undos := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
			plan, err := undos.BuildPlan(child, write, write, "")
			require.NoError(t, err)
			restored, deleted, err := undos.Apply(ctx, child, plan)
			require.NoError(t, err)
			require.Equal(t, 1, restored)
			require.Zero(t, deleted)
			child, err = h.branches.GetBranchByID(child.ID)
			require.NoError(t, err)
			after, err := h.mat.MaterializeDocumentAtLSN(child, editCollection, key, child.HeadLSN)
			require.NoError(t, err)
			assert.Equal(t, point, after)
		})
	}
}

// A published floor excludes retained rows from the batch shortcut. The
// singular reader still answers a typed key directly while deletion is pending.
func TestSource_EditPointWithRetainedRowBelowFloor(t *testing.T) {
	h := newHistoryFixture(t, "edit-retained-row")
	ctx := t.Context()
	const key = "item"
	a := bson.M{"_id": key, "version": "A"}
	first, err := h.writer.Put(ctx, editCollection, a)
	require.NoError(t, err)
	last, err := h.writer.Put(ctx, editCollection, bson.M{"_id": key, "version": "B"})
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, editCollection, last))
	point, err := h.mat.MaterializeDocumentAtLSN(h.main, editCollection, key, first)
	require.NoError(t, err)
	require.Equal(t, a, point)
	_, err = h.mat.MaterializeDocumentsAtLSN(h.main, editCollection, []string{key}, first)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
}

// Record the actual inputs separately from the readers. A delete, untrusted
// write, or duplicate still shadows earlier writes for this document.
type editImageRecord struct {
	branch, key string
	at          int64
	raw         bson.Raw
	frame       bool
	ordered     bool
	untrusted   bool
}

type editImageSources struct {
	row, frame editImageRecord
}

func visibleEditImages(branch *wal.Branch, pos int64, parents map[string]*wal.Branch, records []editImageRecord) map[string]editImageSources {
	visible := make(map[string]editImageSources)
	for branch != nil {
		for _, r := range records {
			if r.branch != branch.ID || r.at > pos {
				continue
			}
			s := visible[r.key]
			if r.frame && r.at > s.frame.at {
				s.frame = r
			}
			if !r.frame && r.at > s.row.at {
				s.row = r
			}
			visible[r.key] = s
		}
		pos = min(pos, branch.BaseLSN)
		branch = parents[branch.ParentID]
	}
	return visible
}

func (s editImageSources) checkBytes(t *testing.T, doc bson.M, got bson.Raw, label string) {
	t.Helper()
	// Name-sorted snapshots may recover field order from a clean, matching
	// WAL write. At the same position, also check a new snapshot against its
	// source write so sorting during snapshot creation cannot pass unnoticed.
	candidates := []bson.Raw{s.row.raw, s.frame.raw}
	if s.row.at > s.frame.at {
		candidates = candidates[:1]
	} else if s.frame.ordered && s.frame.at > s.row.at {
		candidates = candidates[1:]
	}
	if s.row.untrusted {
		if s.row.at > s.frame.at && doc != nil {
			sorted, err := wal.MarshalDocument(doc)
			require.NoError(t, err)
			require.Equal(t, sorted, got, "%s untrusted post-image", label)
		}
		candidates = []bson.Raw{s.frame.raw}
		if s.frame.at < s.row.at {
			return
		}
	}
	for _, raw := range candidates {
		if raw == nil || hasRepeatedEditField(t, raw) {
			continue
		}
		var stored bson.M
		require.NoError(t, wal.UnmarshalDocument(raw, &stored))
		equal, err := mongoexpr.CanonicalEqual(doc, stored)
		require.NoError(t, err)
		if equal {
			require.Equal(t, raw, got, "%s clean matching image", label)
			return
		}
	}
}

func checkEditImagePosition(t *testing.T, h *historyFixture, branch *wal.Branch, pos int64, sources map[string]editImageSources) {
	t.Helper()
	label := fmt.Sprintf("%s at %d", branch.Name, pos)
	full, fullErr := h.mat.MaterializeCollectionAtLSN(branch, editCollection, pos)
	images, err := h.mat.EditCollectionAtLSN(branch, editCollection, pos)
	require.Equal(t, fullErr == nil, err == nil, "%s collection: decoded %v, ordered %v", label, fullErr, err)
	if fullErr != nil {
		return
	}
	require.Len(t, images, len(full), label)
	for key, doc := range full {
		raw, ok := images[key]
		require.True(t, ok, "%s missing key %q", label, key)
		require.Equal(t, doc, decodeEditImage(t, raw), "%s collection key %q", label, key)
		sources[key].checkBytes(t, doc, raw, label+" collection "+key)
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
	require.False(t, hasRepeatedEditField(t, raw), "repeated field in %v", raw)
}

func hasRepeatedEditField(t *testing.T, raw bson.Raw) bool {
	t.Helper()
	fields, err := raw.Elements()
	require.NoError(t, err)
	seen := make(map[string]bool)
	for _, f := range fields {
		if seen[f.Key()] {
			return true
		}
		seen[f.Key()] = true
		v := f.Value()
		if v.Type == bson.TypeEmbeddedDocument || v.Type == bson.TypeArray {
			if hasRepeatedEditField(t, v.Value) {
				return true
			}
		}
	}
	return false
}
