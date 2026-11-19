package materializer

import (
	"bytes"
	"errors"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// EditCollectionAtLSN replays one collection into BSON images. The state holds
// one representation of each document. Decoded maps are needed only for an
// individual legacy or unverified put, or for a value-only snapshot provider.
func (s *Service) EditCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.Raw, error) {
	snapshot := func(b *wal.Branch, coll string, from, to int64) (map[string]bson.Raw, int64, bool, error) {
		return s.editSnapshot(b, coll, from, to, nil)
	}
	return replayCollection(s, branch, collection, targetLSN, snapshot, applyEditEntry)
}

func applyEditEntry(state map[string]bson.Raw, entry *wal.Entry) error {
	return applyEntry(state, entry, editPut, func(raw bson.Raw) any {
		id, _ := wal.DocumentIDFromImage(raw)
		return id
	})
}

func editPut(entry *wal.Entry, id any, hasID bool) (bson.Raw, error) {
	if hasID && entry.DocumentKeyVersion > 0 && entry.Metadata["capture_images"] != "attached_unverified" {
		return entry.PostImage, nil
	}
	doc, err := decodePut(entry, id, hasID)
	if err != nil {
		return nil, err
	}
	return wal.MarshalDocument(doc)
}

// EditDocumentAtLSN returns the same BSON as checkout for one document,
// or nil when it does not exist. A retained WAL row answers in one hop.
func (s *Service) EditDocumentAtLSN(branch *wal.Branch, collection, key string, targetLSN int64) (bson.Raw, error) {
	segments, err := s.collectionSegments(branch, collection, targetLSN)
	if err != nil {
		return nil, err
	}
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		visible := func(lsn int64) bool { return !seg.branch.IsDiscardedForRead(lsn, seg.toLSN) }
		legacy, err := s.wal.HasLegacyEditKeys(seg.branch.ID, collection, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, err
		}
		if legacy {
			// Old envelopes can name an unrelated key. Only a replay sees
			// every image and resolves deletes against the complete state.
			state, err := s.EditCollectionAtLSN(branch, collection, targetLSN)
			return state[key], err
		}
		entry, err := s.wal.LatestDocumentImageEntry(seg.branch.ID, collection, key, seg.fromLSN, seg.toLSN, visible)
		if err != nil {
			return nil, err
		}
		floor, err := s.DocumentImageReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return nil, err
		}
		if entry != nil && entry.LSN > floor {
			state := make(map[string]bson.Raw, 1)
			if err := applyEditEntry(state, entry); err != nil {
				return nil, err
			}
			return state[key], nil
		}
		if s.snapshots != nil {
			frames, lsn, ok, err := s.editSnapshot(seg.branch, collection, seg.fromLSN, seg.toLSN, &key)
			if err != nil {
				return nil, err
			}
			if ok && !needsReclaimed(seg, lsn+1, floor) {
				return frames[key], nil
			}
		}
		if needsReclaimed(seg, seg.fromLSN, floor) {
			return nil, reclaimedError(seg.branch, collection, floor, targetLSN)
		}
	}
	return nil, nil
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
		if err != nil || !found || ordered {
			return frames, lsn, found, err
		}
	} else {
		docs, at, ok, err := s.decodedSnapshot(branch, collection, from, to)
		if err != nil || !ok {
			return nil, at, ok, err
		}
		lsn, found = at, true
		frames = make(map[string]bson.Raw)
		for k, doc := range docs {
			if key != nil && k != *key {
				continue
			}
			frames[k], err = wal.MarshalDocument(doc)
			if err != nil {
				return nil, 0, false, err
			}
			// Release each decoded document as its BSON takes its place.
			delete(docs, k)
		}
	}
	if err := s.recoverSnapshotOrder(branch, collection, lsn, frames); err != nil {
		return nil, 0, false, err
	}
	return frames, lsn, found, nil
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
		floor, err := s.DocumentImageReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return err
		}
		start := max(seg.fromLSN, floor+1)
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
				if e.Operation == wal.OpPut && e.DocumentKeyVersion > 0 && e.Metadata["capture_images"] != "attached_unverified" {
					if _, err := wal.DocumentIDFromImage(e.PostImage); err == nil {
						frames[key] = e.PostImage
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
					frames[key] = bytes.Clone(raw)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		// A floor is published before rows disappear. Retry the snapshot
		// read if it changed during the scan rather than expose stale bytes.
		after, err := s.DocumentImageReclaimFloor(seg.branch.ID, collection)
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
