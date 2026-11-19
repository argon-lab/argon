package materializer

import (
	"bytes"
	"errors"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// EditCollectionAtLSN replays one collection into BSON images. The state holds
// one representation of each document. Legacy captures and gaps in the
// retained history can still supply decoded values without proving order.
func (s *Service) EditCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.Raw, error) {
	snapshot := func(b *wal.Branch, coll string, from, to int64) (map[string]bson.Raw, int64, bool, error) {
		return s.editSnapshot(b, coll, from, to, nil)
	}
	images, err := replayCollection(s, branch, collection, targetLSN, snapshot, func(state map[string]bson.Raw, seg segment) error {
		return s.replayEditSegment(collection, state, seg)
	})
	if err == nil {
		return images, nil
	}
	// Recovering original field order consults extra WAL and snapshot history.
	// Its absence must not make an otherwise readable collection unavailable.
	docs, err := s.MaterializeCollectionAtLSN(branch, collection, targetLSN)
	if err != nil {
		return nil, err
	}
	return encodeEditDocuments(docs, nil)
}

// A pre-2.1.6 manifest bounds possible unrecorded deletions on this branch.
func (s *Service) unrecordedEditBound(branch *wal.Branch, collection string) (int64, error) {
	if s.snapshots == nil {
		return 0, nil
	}
	return s.snapshots.UnrecordedReclaimBound(branch.ID, collection)
}

func (s *Service) replayEditSegment(collection string, state map[string]bson.Raw, seg segment) error {
	bound, err := s.unrecordedEditBound(seg.branch, collection)
	if err != nil {
		return err
	}
	if !needsReclaimed(seg, seg.fromLSN, bound) {
		return scanSegment(s, collection, state, seg, applyEditEntry)
	}
	// Check coverage after replay: an old GC might remove a row while we
	// read. A surviving write above the last missing position proves its
	// order. Any other document could have been replaced at that position.
	lastPut := make(map[string]int64)
	put := func(e *wal.Entry, id any, hasID bool) (bson.Raw, error) {
		key := e.DocumentID
		if hasID {
			key = wal.DocumentIDString(id)
		}
		lastPut[key] = e.LSN
		return editPut(e, id, hasID)
	}
	err = scanSegment(s, collection, state, seg, func(state map[string]bson.Raw, e *wal.Entry) error {
		return applyEntry(state, e, put, editImageID)
	})
	if err != nil {
		return err
	}
	gap, err := s.wal.LastMissingVisibleLSN(seg.branch, seg.fromLSN, seg.toLSN, bound)
	if err != nil || gap == 0 {
		return err
	}
	for key, raw := range state {
		if lastPut[key] < gap {
			state[key], err = sortedEditImage(raw)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func editImageID(raw bson.Raw) any {
	id, _ := wal.DocumentIDFromImage(raw)
	return id
}

func applyEditEntry(state map[string]bson.Raw, entry *wal.Entry) error {
	return applyEntry(state, entry, editPut, editImageID)
}

func editPut(entry *wal.Entry, id any, hasID bool) (bson.Raw, error) {
	if hasID && entry.HasOrderedEditImage() {
		image, _, err := wal.NormalizeEditImage(entry.PostImage)
		return image, err
	}
	return encodeDecodedPut(entry, id, hasID)
}

func encodeDecodedPut(entry *wal.Entry, id any, hasID bool) (bson.Raw, error) {
	doc, err := decodePut(entry, id, hasID)
	if err != nil {
		return nil, err
	}
	return wal.MarshalDocument(doc)
}

// sortedEditImage encodes the value exposed by decoded replay, which takes the
// first top-level _id verbatim (including the order of a compound key).
func sortedEditImage(raw bson.Raw) (bson.Raw, error) {
	if raw == nil {
		return nil, nil
	}
	var doc bson.M
	if err := wal.UnmarshalDocument(raw, &doc); err != nil {
		return nil, err
	}
	if id, err := wal.DocumentIDFromImage(raw); err == nil {
		doc["_id"] = id
	}
	return wal.MarshalDocument(doc)
}

// EditDocumentAtLSN returns an established edit image or the encoded decoded
// value. A retained typed entry normally answers without replaying the collection.
func (s *Service) EditDocumentAtLSN(branch *wal.Branch, collection, key string, targetLSN int64) (bson.Raw, error) {
	image, known, err := s.lookupEditDocument(branch, collection, key, targetLSN)
	if known && err == nil {
		return image, nil
	}

	// A descendant's snapshot can cover a gap on an ancestor. Conversely a
	// decoded point read can settle one key even when a full replay fails.
	// Use the same fallback to collection replay as other decoded point callers.
	docs, err := s.MaterializeDocumentsAtLSN(branch, collection, []string{key}, targetLSN)
	if err != nil {
		images, err := s.EditCollectionAtLSN(branch, collection, targetLSN)
		return images[key], err
	}
	if docs[key] == nil {
		return nil, nil
	}
	decoded, err := wal.MarshalDocument(docs[key])
	if err != nil {
		return nil, err
	}
	// The point reader may answer from an ancestor even when collection
	// replay uses a descendant's frame. Keep that frame only if they agree.
	if image != nil {
		sorted, err := sortedEditImage(image)
		if err == nil && bytes.Equal(sorted, decoded) {
			return image, nil
		}
	}
	return decoded, nil
}

func (s *Service) lookupEditDocument(branch *wal.Branch, collection, key string, targetLSN int64) (bson.Raw, bool, error) {
	segments, err := s.collectionSegments(branch, collection, targetLSN)
	if err != nil {
		return nil, false, err
	}
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		visible := func(lsn int64) bool { return !seg.branch.IsDiscardedForRead(lsn, seg.toLSN) }
		legacy, err := s.wal.HasLegacyEditKeys(seg.branch.ID, collection, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, false, err
		}
		if legacy {
			// Old envelopes can name an unrelated key. Only a replay sees
			// every image and resolves deletes against the complete state.
			state, err := s.EditCollectionAtLSN(branch, collection, targetLSN)
			return state[key], true, err
		}
		entry, err := s.wal.LatestDocumentImageEntry(seg.branch.ID, collection, key, seg.fromLSN, seg.toLSN, visible)
		if err != nil {
			return nil, false, err
		}
		floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return nil, false, err
		}
		from := seg.fromLSN
		retained := entry != nil && entry.LSN > floor
		if retained {
			from = entry.LSN + 1
		}
		gap, err := s.unrecordedGap(seg, collection, from)
		if err != nil {
			return nil, false, err
		}
		if retained && gap == 0 {
			state := make(map[string]bson.Raw, 1)
			if err := applyEditEntry(state, entry); err != nil {
				return nil, false, err
			}
			raw := state[key]
			if s.snapshots != nil && raw != nil && entry.HasOrderedEditImage() && !bytes.Equal(raw, entry.PostImage) {
				// A value-only snapshot may already hold the corrected document.
				// Preserve its order just as collection replay does.
				frames, _, ok, err := s.editSnapshot(seg.branch, collection, entry.LSN, seg.toLSN, &key)
				if err == nil && ok {
					return frames[key], false, nil
				}
			}
			return raw, true, nil
		}
		bound := max(floor, gap)
		if s.snapshots != nil {
			frames, lsn, ok, err := s.editSnapshot(seg.branch, collection, seg.fromLSN, seg.toLSN, &key)
			if err != nil {
				return nil, false, err
			}
			if ok && !needsReclaimed(seg, lsn+1, bound) {
				if !retained && !needsReclaimed(seg, seg.fromLSN, bound) {
					// A snapshot can be the only record after a direct WAL
					// deletion without a floor. Only a possible unrecorded
					// deletion on an ancestor can leave a competing point value.
					for _, ancestor := range segments[:i] {
						hole, err := s.unrecordedGap(ancestor, collection, ancestor.fromLSN)
						if err != nil || hole != 0 {
							return frames[key], false, err
						}
					}
					return frames[key], true, nil
				}
				return frames[key], false, nil
			}
		}
		if needsReclaimed(seg, seg.fromLSN, bound) {
			return nil, false, nil
		}
	}
	return nil, true, nil
}

func (s *Service) unrecordedGap(seg segment, collection string, from int64) (int64, error) {
	if from > seg.toLSN {
		return 0, nil
	}
	bound, err := s.unrecordedEditBound(seg.branch, collection)
	if err != nil {
		return 0, err
	}
	return s.wal.LastMissingVisibleLSN(seg.branch, from, seg.toLSN, bound)
}

// A snapshot can recover order across a recorded floor or a possible legacy
// deletion only when the candidate covers the last such visible position.
func (s *Service) snapshotEditBound(seg segment, collection string) (int64, error) {
	gap, err := s.unrecordedGap(seg, collection, seg.fromLSN)
	if err != nil {
		return 0, err
	}
	floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
	return max(gap, floor), err
}

// A frame provider reads the selected snapshot's BSON without decoding it.
// The second boolean distinguishes snapshots produced from edit images from
// older snapshots that encoded decoded, name-sorted fields.
type frameSource interface {
	FindUsableFrames(branch *wal.Branch, collection string, from, to int64, key *string) (map[string]bson.Raw, int64, bool, bool, error)
}

func (s *Service) editSnapshot(branch *wal.Branch, collection string, from, to int64, key *string) (map[string]bson.Raw, int64, bool, error) {
	var frames map[string]bson.Raw
	var lsn int64
	var found bool
	if source, ok := s.snapshots.(frameSource); ok {
		var ordered bool
		var err error
		frames, lsn, found, ordered, err = source.FindUsableFrames(branch, collection, from, to, key)
		if err != nil || !found {
			return nil, lsn, found, err
		}
		for k, raw := range frames {
			frames[k], _, err = wal.NormalizeEditImage(raw)
			if err != nil {
				return nil, 0, false, err
			}
		}
		if ordered {
			return frames, lsn, true, nil
		}
	} else {
		docs, at, ok, err := s.decodedSnapshot(branch, collection, from, to)
		if err != nil || !ok {
			return nil, at, ok, err
		}
		lsn, found = at, true
		frames, err = encodeEditDocuments(docs, key)
		if err != nil {
			return nil, 0, false, err
		}
	}
	if err := s.recoverSnapshotOrder(branch, collection, lsn, frames); err != nil {
		return nil, 0, false, err
	}
	return frames, lsn, found, nil
}

func encodeEditDocuments(docs map[string]bson.M, key *string) (map[string]bson.Raw, error) {
	frames := make(map[string]bson.Raw)
	for k, doc := range docs {
		if key != nil && k != *key {
			continue
		}
		raw, err := wal.MarshalDocument(doc)
		if err != nil {
			return nil, err
		}
		frames[k] = raw
		delete(docs, k)
	}
	return frames, nil
}

// An ordered frame can cover history already removed from the WAL. Selecting
// its manifest separately lets the caller first check the complete delta and
// stream only the frames it still needs.
type orderedFrameSource interface {
	OrderedSnapshotFrames(branch *wal.Branch, collection string, from, to, readUpperBound int64) (int64, func(func(string, bson.Raw) error) error, error)
}

// Earlier snapshots sorted their documents on creation. Walk each retained
// segment once, newest first. Only a document's latest visible event can
// supply its bytes. Even a legacy row indexed under an unrelated key shadows
// older images because the scan resolves its identity from the BSON itself.
func (s *Service) recoverSnapshotOrder(branch *wal.Branch, collection string, lsn int64, frames map[string]bson.Raw) error {
	segments, err := s.collectionSegments(branch, collection, lsn)
	if err != nil {
		return err
	}
	pending := make(map[string]bool, len(frames))
	for key := range frames {
		pending[key] = true
	}
	stop := errors.New("no more image history needed")
	for i := len(segments) - 1; i >= 0 && len(pending) > 0; i-- {
		seg := segments[i]
		floor, err := s.snapshotEditBound(seg, collection)
		if err != nil {
			return err
		}
		start := max(seg.fromLSN, lastReclaimableLSN(seg, floor))
		var walk func(func(string, bson.Raw) error) error
		if source, ok := s.snapshots.(orderedFrameSource); ok {
			at, visit, err := source.OrderedSnapshotFrames(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
			if err != nil {
				return err
			}
			if visit != nil && !needsReclaimed(seg, at+1, floor) {
				// Every visible change above this checkpoint is still readable.
				// Check it first, including unverified or untyped replacements.
				start, walk = at+1, visit
			}
		}
		err = s.wal.ScanCollectionDataEntries(seg.branch.ID, collection, start, seg.toLSN, true, func(e *wal.Entry) error {
			if seg.branch.IsDiscardedForRead(e.LSN, seg.toLSN) {
				return nil
			}
			key, known := EntryStateKey(e)
			switch {
			case known && pending[key]:
				delete(pending, key)
				if e.Operation == wal.OpPut && e.HasOrderedEditImage() {
					if _, err := wal.DocumentIDFromImage(e.PostImage); err == nil {
						recoverEditFrame(frames, key, e.PostImage)
					}
				}
			case !known && e.Operation == wal.OpDelete && e.DocumentID != "":
				for k := range pending {
					id, err := wal.DocumentIDValue(k)
					if err == nil && wal.LegacyDocumentIDString(id) == e.DocumentID {
						delete(pending, k)
					}
				}
			case !known:
				// No image or key ties this operation to one document.
				clear(pending)
			}
			if len(pending) == 0 {
				return stop
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			return err
		}
		if walk != nil && len(pending) > 0 {
			err := walk(func(key string, raw bson.Raw) error {
				if pending[key] {
					recoverEditFrame(frames, key, raw)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		// A floor is published before rows disappear. Retry the snapshot
		// read if it changed during the scan rather than expose stale bytes.
		after, err := s.snapshotEditBound(seg, collection)
		if err != nil {
			return err
		}
		if after > floor {
			return wal.ErrHistoryReclaimed
		}
		if walk != nil || needsReclaimed(seg, seg.fromLSN, floor) {
			break
		}
	}
	return nil
}

// A value-only frame is authoritative. Recover only the field order from a
// clean candidate with the same decoded contents. The candidate may be lent
// by a chunk reader, so retain an independent copy.
func recoverEditFrame(frames map[string]bson.Raw, key string, candidate bson.Raw) {
	if bytes.Equal(frames[key], candidate) {
		return
	}
	_, changed, err := wal.NormalizeEditImage(candidate)
	if err != nil || changed {
		return
	}
	candidateValues, err := sortedEditImage(candidate)
	if err != nil {
		return
	}
	frameValues, err := sortedEditImage(frames[key])
	if err == nil && bytes.Equal(frameValues, candidateValues) {
		frames[key] = bytes.Clone(candidate)
	}
}
