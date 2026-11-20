package wal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

type editImageHistory struct {
	*historyFixture
	readBranches                            []*wal.Branch
	parents                                 map[string]*wal.Branch
	images                                  []editImageRecord
	keys                                    []string
	missing, legacySnapshot, reclaimThrough int64
}

func newEditImageHistory(t *testing.T, seed int64) *editImageHistory {
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
	legacySnapshot := at
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
	images[len(images)-1].unindexed = true
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

	return &editImageHistory{
		historyFixture: h, readBranches: []*wal.Branch{h.main, early, late, grandchild},
		parents: parents, images: images, keys: keys, missing: missing,
		legacySnapshot: legacySnapshot, reclaimThrough: reclaimThrough,
	}
}

func checkEditImageHistory(t *testing.T, seed int64) {
	t.Helper()
	h := newEditImageHistory(t, seed)
	check := func(t *testing.T) {
		t.Helper()
		for _, branch := range h.readBranches {
			t.Run(branch.Name, func(t *testing.T) {
				t.Parallel()
				for pos := int64(0); pos <= branch.HeadLSN; pos++ {
					sources := visibleEditImages(branch, pos, h.parents, h.images)
					checkEditImagePosition(t, h.historyFixture, branch, pos, sources)
				}
			})
		}
	}
	t.Run("intact", check)
	h.removeRows(t, h.missing)
	t.Run("unrecorded reclamation", check)
	require.NoError(t, h.wal.RecordReclaimFloor(t.Context(), h.main.ID, editCollection, h.reclaimThrough))
	h.removeThroughFloor(t)
	t.Run("recorded reclamation", check)
}

func (h *editImageHistory) removeRows(t *testing.T, missing ...int64) {
	t.Helper()
	// An interrupted GC can leave an older row in place after a newer
	// write or deletion has disappeared.
	ctx := t.Context()
	result, err := h.db.Collection("wal_log").DeleteMany(ctx, bson.M{"branch_id": h.main.ID, "lsn": bson.M{"$in": missing}})
	require.NoError(t, err)
	require.EqualValues(t, len(missing), result.DeletedCount)
	for i, image := range h.images {
		if !image.frame && slices.Contains(missing, image.at) {
			h.images[i].reclaimed = true
		}
	}
}

func (h *editImageHistory) removeThroughFloor(t *testing.T) {
	t.Helper()
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, editCollection, h.reclaimThrough)
	require.NoError(t, err)
	require.Positive(t, removed)
	for i, image := range h.images {
		if !image.frame && image.branch == h.main.ID && image.at <= h.reclaimThrough {
			h.images[i].reclaimed = true
		}
	}
}

// Check every queried key, including the stored envelope spellings and keys
// that only appear on a sibling or later in history.
func TestSource_DecodedPointReadParity(t *testing.T) {
	for _, seed := range []int64{31, 97} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			h := newEditImageHistory(t, seed)
			h.addLegacyPointRows(t)
			check := func(t *testing.T) { h.checkPointStage(t, false) }
			t.Run("intact", check)
			// Remove the value-changing put at the newer snapshot and the
			// delete at the 2.1.5 snapshot. Both have older surviving puts.
			h.removeRows(t, h.missing, h.legacySnapshot, h.reclaimThrough)
			t.Run("unrecorded reclamation", func(t *testing.T) { h.checkPointStage(t, true) })
			require.NoError(t, h.wal.RecordReclaimFloor(t.Context(), h.main.ID, editCollection, h.reclaimThrough))
			t.Run("floor with retained rows", check)
			h.removeThroughFloor(t)
			t.Run("recorded reclamation", check)
		})
	}
}

func (h *editImageHistory) addLegacyPointRows(t *testing.T) {
	t.Helper()
	early := h.readBranches[1]
	za := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	az := bson.D{za[1], za[0]}
	oldKey := wal.LegacyDocumentIDString(az)
	// The early child already has an opaque row over a missing parent
	// key. Give the same envelope another image that shadows a parent put.
	// The two compound IDs must stay distinct even though the importer's
	// text envelope names the opposite field order. The final delete has
	// only that text key and removes the a-first document by its actual ID.
	for _, row := range []struct {
		id               any
		op               wal.OperationType
		storedKey, value string
	}{
		{h.keys[0], wal.OpPut, "old-envelope", "child"},
		{az, wal.OpPut, "", "a first"},
		{za, wal.OpPut, oldKey, "z first"},
		{az, wal.OpDelete, oldKey, ""},
	} {
		key := wal.DocumentIDString(row.id)
		var raw bson.Raw
		if row.op == wal.OpPut {
			raw = mustMarshalBSON(bson.D{{Key: "_id", Value: row.id}, {Key: "value", Value: row.value}})
		}
		at, err := h.wal.Append(&wal.Entry{ProjectID: h.project, BranchID: early.ID,
			Collection: editCollection, DocumentID: key, Operation: row.op, PostImage: raw})
		require.NoError(t, err)
		if row.storedKey != "" {
			res, err := h.db.Collection("wal_log").UpdateOne(t.Context(), bson.M{"branch_id": early.ID, "lsn": at},
				bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": row.storedKey}})
			require.NoError(t, err)
			require.EqualValues(t, 1, res.ModifiedCount)
		}
		require.NoError(t, h.branches.UpdateBranchHead(early.ID, at))
		early.HeadLSN = at
		h.images = append(h.images, editImageRecord{branch: early.ID, key: key, at: at, raw: raw,
			unindexed: row.storedKey != "" && row.storedKey != wal.LegacyDocumentIDString(row.id)})
	}
	h.keys = append(h.keys, wal.DocumentIDString(za), wal.DocumentIDString(az), "old-envelope", oldKey)
}

type pointReadCounts struct {
	match, different, bothFailed, pointFailed, replayFailed int
	olderIndexed, ambiguousDelete                           int
}

func (h *editImageHistory) checkPointStage(t *testing.T, unrecorded bool) {
	t.Helper()
	byBranch := make([]pointReadCounts, len(h.readBranches))
	t.Run("branches", func(t *testing.T) {
		for i, branch := range h.readBranches {
			t.Run(branch.Name, func(t *testing.T) {
				t.Parallel()
				byBranch[i] = h.checkPointBranch(t, branch, unrecorded)
			})
		}
	})
	var count pointReadCounts
	total := 0
	for i, c := range byBranch {
		count.match += c.match
		count.different += c.different
		count.bothFailed += c.bothFailed
		count.pointFailed += c.pointFailed
		count.replayFailed += c.replayFailed
		count.olderIndexed += c.olderIndexed
		count.ambiguousDelete += c.ambiguousDelete
		total += (int(h.readBranches[i].HeadLSN) + 1) * len(h.keys)
	}
	require.Equal(t, total, count.match+count.different+count.bothFailed+count.pointFailed+count.replayFailed)
	require.Zero(t, count.pointFailed)
	floor, err := h.wal.ReclaimFloor(h.main.ID, editCollection)
	require.NoError(t, err)
	if floor == 0 {
		require.Equal(t, total, count.match+count.different)
	} else {
		require.Positive(t, count.bothFailed)
		require.Positive(t, count.replayFailed)
	}
	t.Logf("matched=%d different=%d both-failed=%d point-only-errors=%d replay-only-reclaimed=%d (older indexed answer=%d, both failed with legacy delete=%d)",
		count.match, count.different, count.bothFailed, count.pointFailed, count.replayFailed, count.olderIndexed, count.ambiguousDelete)
}

func (h *editImageHistory) checkPointBranch(t *testing.T, branch *wal.Branch, unrecorded bool) pointReadCounts {
	t.Helper()
	var counts pointReadCounts
	for pos := int64(0); pos <= branch.HeadLSN; pos++ {
		full, replayErr := h.mat.MaterializeCollectionAtLSN(branch, editCollection, pos)
		for key := range full {
			require.Contains(t, h.keys, key, "%s at %d: missing point query", branch.Name, pos)
		}
		sources := visibleEditImages(branch, pos, h.parents, h.images)
		for _, key := range h.keys {
			got, pointErr := h.mat.MaterializeDocumentAtLSN(branch, editCollection, key, pos)
			label := fmt.Sprintf("%s at %d key %q", branch.Name, pos, key)
			switch {
			case replayErr == nil && pointErr == nil:
				want, present := full[key]
				if present == (got != nil) && assert.ObjectsAreEqual(want, got) {
					counts.match++
					continue
				}
				counts.different++
				src := sources[key]
				// An ordinary retained row still answers when the floor is
				// zero, even if a newer put or delete was manually removed.
				switch {
				case unrecorded && branch.ID == h.main.ID && key == h.keys[0] && pos == h.reclaimThrough:
					assert.Equal(t, src.frame.pointValue(t, label), want, label)
					t.Logf("%s: removed put at %d; replay uses its frame, point uses row at %d", label, h.reclaimThrough, src.row.at)
				case unrecorded && branch.ID == h.main.ID && key == h.keys[1] && pos >= h.legacySnapshot:
					assert.False(t, present, label)
					t.Logf("%s: removed delete at %d; replay omits the key, point uses row at %d", label, h.legacySnapshot, src.row.at)
				default:
					t.Errorf("%s: replay returned %v, point returned %v", label, want, got)
					continue
				}
				assert.Equal(t, src.row.pointValue(t, label), got, label)
			case replayErr == nil:
				counts.pointFailed++
				t.Errorf("%s: point failed on readable history: %v", label, pointErr)
			case pointErr != nil:
				counts.bothFailed++
				require.ErrorIs(t, replayErr, wal.ErrHistoryReclaimed, label)
				if errors.Is(pointErr, wal.ErrLegacyDocumentKeys) {
					// The indexed lookup cannot resolve the final image-less
					// compound delete once the parent range is unavailable.
					counts.ambiguousDelete++
					require.Equal(t, "early", branch.Name, label)
					require.Equal(t, branch.HeadLSN, pos, label)
				} else {
					require.ErrorIs(t, pointErr, wal.ErrHistoryReclaimed, label)
				}
			default:
				// A surviving full put or delete can answer a single key
				// even when replay needs an uncovered reclaimed range.
				counts.replayFailed++
				require.ErrorIs(t, replayErr, wal.ErrHistoryReclaimed, label)
				src := sources[key]
				want := src.row.pointValue(t, label)
				if src.row.unindexed && !assert.ObjectsAreEqual(want, got) {
					// With no readable collection, the preserved point
					// shortcut may still reach an older indexed put/delete.
					counts.olderIndexed++
					want = src.indexed.pointValue(t, label)
				}
				require.Equal(t, want, got, label)
			}
		}
	}
	return counts
}

// A typed row can still establish a key when an unrelated historical delete
// prevents replay of the collection, including on a segment with untyped data.
func TestSource_DecodedPointWithUnrelatedAmbiguousDelete(t *testing.T) {
	h := newHistoryFixture(t, "unrelated-ambiguous-delete")
	want := bson.M{"_id": "stable", "value": "retained"}
	_, err := h.writer.Put(t.Context(), editCollection, want)
	require.NoError(t, err)
	other, err := h.writer.Put(t.Context(), editCollection, bson.M{"_id": "other"})
	require.NoError(t, err)
	res, err := h.db.Collection("wal_log").UpdateOne(t.Context(), bson.M{"branch_id": h.main.ID, "lsn": other},
		bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": "opaque-envelope"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.ModifiedCount)
	shared := oldSharedIndexKey(t, primitive.Binary{Subtype: 2, Data: make([]byte, 4)})
	at := insertOldBinaryEntry(t, h.db, h.main, editCollection, shared, wal.OpDelete, nil)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	_, err = h.mat.MaterializeCollectionAtLSN(h.main, editCollection, at)
	var ambiguous *wal.AmbiguousOldBinaryError
	require.ErrorAs(t, err, &ambiguous)
	require.Equal(t, at, ambiguous.LSN)
	point, err := h.mat.MaterializeDocumentAtLSN(h.main, editCollection, "stable", at)
	require.NoError(t, err)
	require.Equal(t, want, point)
}

func TestSource_DecodedPointSeparateSnapshotDatabase(t *testing.T) {
	for _, name := range []string{"manifests and chunks", "chunks only"} {
		t.Run(name, func(t *testing.T) {
			h := newHistoryFixture(t, "point-split")
			ctx := t.Context()
			other := h.db.Client().Database("point_chunks_" + primitive.NewObjectID().Hex())
			t.Cleanup(func() { require.NoError(t, other.Drop(context.Background())) })
			manifests := h.db
			if name == "manifests and chunks" {
				manifests = other
			}
			store := snapshot.NewMongoChunkStore(other)
			snaps, err := snapshot.NewServiceWithStore(manifests, h.branches, h.mat, store)
			require.NoError(t, err)
			want := bson.M{"_id": "q", "version": "B"}
			at, err := h.writer.Put(ctx, editCollection, want)
			require.NoError(t, err)
			branch := h.reload()
			created, err := snaps.CreateSnapshot(ctx, branch.ID, at)
			require.NoError(t, err)
			require.Len(t, created, 1)
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, branch.ID, editCollection, at))
			_, err = h.wal.DeleteDataEntriesUpTo(branch.ID, editCollection, at)
			require.NoError(t, err)
			state, err := h.mat.MaterializeCollection(branch, editCollection)
			require.NoError(t, err)
			require.Equal(t, want, state["q"])
			got, err := h.mat.MaterializeDocument(branch, editCollection, "q")
			require.NoError(t, err)
			require.Equal(t, state["q"], got)

			// A copy in the WAL database cannot make the configured store
			// readable after one of its referenced chunks disappears.
			require.Len(t, created[0].ChunkIDs, 1)
			id := created[0].ChunkIDs[0]
			data, err := store.Get(ctx, id)
			require.NoError(t, err)
			copied, err := snapshot.NewMongoChunkStore(h.db).Put(ctx, data)
			require.NoError(t, err)
			require.Equal(t, id, copied)
			require.NoError(t, store.Delete(ctx, []string{id}))
			_, err = h.mat.MaterializeCollection(branch, editCollection)
			require.ErrorContains(t, err, id)
			_, err = h.mat.MaterializeDocument(branch, editCollection, "q")
			require.ErrorContains(t, err, id)
		})
	}
}

func TestSource_DecodedPointAcrossSnapshotChunks(t *testing.T) {
	h := newHistoryFixture(t, "point-chunks")
	ctx := t.Context()
	store := snapshot.NewMongoChunkStore(h.db)
	z, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, z.Close()) })

	frame := func(envelope, id, version string) bson.Raw {
		doc := bson.D{{Key: "_id", Value: id}, {Key: "version", Value: version}}
		return mustMarshalBSON(bson.D{{Key: "k", Value: envelope}, {Key: "d", Value: doc}})
	}
	var chunks []string
	var count, size int
	for i := range 9 {
		id := fmt.Sprintf("item-%d", i)
		frames := []bson.Raw{frame(id, id, "item")}
		// Both end chunks span m. Only the first has c, and the last
		// supplies two newer revisions of m under different frame keys.
		switch i {
		case 0:
			frames = []bson.Raw{frame("alias-c", "c", "only"), frame("alias-m", "m", "first")}
		case 8:
			frames = []bson.Raw{frame("a", "a", "lower"), frame("z", "z", "upper"),
				frame("middle", "m", "second"), frame("last", "m", "final")}
		}
		var stream []byte
		for _, raw := range frames {
			stream = append(stream, raw...)
		}
		data, err := z.Compress(stream)
		require.NoError(t, err)
		chunk, err := store.Put(ctx, data)
		require.NoError(t, err)
		chunks = append(chunks, chunk)
		count += len(frames)
		size += len(data)
	}
	_, err = h.db.Collection("wal_snapshots").InsertOne(ctx, bson.M{
		"project_id": h.project, "branch_id": h.main.ID, "collection": editCollection, "lsn": h.main.HeadLSN,
		"visibility_rules": snapshot.CurrentVisibilityRules, "exact_old_binary_keys": true,
		"chunk_ids": chunks, "doc_count": count, "size_bytes": size,
	})
	require.NoError(t, err)
	state, err := h.mat.MaterializeCollection(h.main, editCollection)
	require.NoError(t, err)
	require.Equal(t, "final", state["m"]["version"])
	require.Equal(t, "only", state["c"]["version"])
	require.NotContains(t, state, "alias-c")
	require.NotContains(t, state, "g")

	for _, key := range []string{"m", "c", "g", "alias-c", "item-3"} {
		point, err := h.mat.MaterializeDocument(h.main, editCollection, key)
		require.NoError(t, err, key)
		require.Equal(t, state[key], point, key)
	}
	// A different missing chunk makes the snapshot unreadable to both readers.
	require.NoError(t, store.Delete(ctx, []string{chunks[5]}))
	_, err = h.mat.MaterializeCollection(h.main, editCollection)
	require.ErrorContains(t, err, chunks[5])
	point, err := h.mat.MaterializeDocument(h.main, editCollection, "item-3")
	require.ErrorContains(t, err, chunks[5])
	require.Nil(t, point)
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
	reclaimed   bool
	unindexed   bool
}

func (r editImageRecord) pointValue(t *testing.T, label string) bson.M {
	t.Helper()
	require.NotZero(t, r.at, label)
	if r.raw == nil {
		return nil
	}
	doc, err := wal.DecodeImage(r.raw)
	require.NoError(t, err, label)
	return doc.TakeValue()
}

type editImageSources struct {
	row, indexed, frame editImageRecord
	reclaimed           []int64
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
			if r.reclaimed {
				s.reclaimed = append(s.reclaimed, r.at)
			} else if !r.frame {
				if r.at > s.row.at {
					s.row = r
				}
				if !r.unindexed && r.at > s.indexed.at {
					s.indexed = r
				}
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
	// A deleted write after the surviving row can change field order
	// without changing any decoded values in the later frame.
	reclaimedBetween := slices.ContainsFunc(s.reclaimed, func(at int64) bool {
		return s.row.at < at && at <= s.frame.at
	})
	if s.row.at > s.frame.at {
		candidates = candidates[:1]
	} else if reclaimedBetween || (s.frame.ordered && s.frame.at > s.row.at) {
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
