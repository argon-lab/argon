package materializer

import (
	"bytes"
	"errors"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// EditCollectionAtLSN uses the decoded collection replay, decorating each put
// or snapshot frame as it is read. Only its BSON representation stays in state.
func (s *Service) EditCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.Raw, error) {
	return replayCollection(s, branch, collection, targetLSN, s.editSnapshot, replayValue[bson.Raw]{
		store: func(doc *wal.DecodedImage, e *wal.Entry) (bson.Raw, error) {
			return editImage(doc, doc, wal.PostImage, e)
		},
		id: func(raw bson.Raw) any {
			id, _ := wal.DocumentIDFromImage(raw)
			return id
		},
	})
}

func (s *Service) editSnapshot(branch *wal.Branch, collection string, from, to int64) (map[string]bson.Raw, int64, bool, error) {
	frames := make(map[string]bson.Raw)
	lsn, found, ordered, err := s.visitSnapshotFrames(branch, collection, from, to, func() { clear(frames) }, func(k string, doc *wal.DecodedImage) error {
		image, err := editImage(doc, doc, wal.SnapshotImage, nil)
		if err == nil {
			frames[k] = bytes.Clone(image)
		}
		return err
	})
	if err != nil || !found || ordered {
		return frames, lsn, found, err
	}
	s.recoverSnapshotOrder(branch, collection, lsn, frames)
	return frames, lsn, found, nil
}

type orderedFrameSource interface {
	OrderedSnapshotFrames(branch *wal.Branch, collection string, from, to, readUpperBound int64) (int64, func(func(string, bson.Raw) error) error, error)
}

// Earlier snapshots encoded fields in name order. Try each key's latest
// retained WAL image or an earlier ordered frame. Missing candidates affect
// only field order. The selected snapshot has already supplied every value.
func (s *Service) recoverSnapshotOrder(branch *wal.Branch, collection string, lsn int64, frames map[string]bson.Raw) {
	segments, err := s.collectionSegments(branch, collection, lsn)
	if err != nil {
		return
	}
	pending := make(map[string]bool, len(frames))
	for key := range frames {
		pending[key] = true
	}
	stop := errors.New("all frame orders considered")
	for i := len(segments) - 1; i >= 0 && len(pending) > 0; i-- {
		seg := segments[i]
		start := seg.fromLSN
		var walk func(func(string, bson.Raw) error) error
		if source, ok := s.snapshots.(orderedFrameSource); ok {
			// A usable ordered copy at the selected LSN would have won
			// the tie. It cannot supply order after its chunks failed.
			at, visit, err := source.OrderedSnapshotFrames(seg.branch, collection, start, min(seg.toLSN, lsn-1), seg.toLSN)
			if err != nil {
				return
			}
			if visit != nil {
				start, walk = at+1, visit
			}
		}
		err := s.wal.ScanCollectionDataEntries(seg.branch.ID, collection, start, seg.toLSN, true, func(e *wal.Entry) error {
			if seg.branch.IsDiscardedForRead(e.LSN, seg.toLSN) {
				return nil
			}
			key, known := EntryStateKey(e)
			if !known {
				if e.Operation != wal.OpDelete || e.DocumentID == "" {
					return stop
				}
				for k := range pending {
					id, err := wal.DocumentIDValue(k)
					if err == nil && wal.LegacyDocumentIDString(id) == e.DocumentID {
						delete(pending, k)
					}
				}
			} else if pending[key] {
				delete(pending, key)
				if e.Operation == wal.OpPut {
					recoverEditFrame(frames, key, e.PostImage, wal.PostImage, e)
				}
			}
			if len(pending) == 0 {
				return stop
			}
			return nil
		})
		if err != nil {
			return
		}
		// Once intervening writes have been reclaimed, prefer the selected
		// frame's own order to a possibly superseded older frame.
		floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
		if err != nil || needsReclaimed(seg, start, floor) {
			return
		}
		if walk != nil {
			_ = walk(func(k string, raw bson.Raw) error {
				if pending[k] {
					recoverEditFrame(frames, k, raw, wal.SnapshotImage, nil)
				}
				return nil
			})
			return
		}
	}
}

func recoverEditFrame(frames map[string]bson.Raw, key string, candidate bson.Raw, role wal.ImageRole, entry *wal.Entry) {
	if bytes.Equal(frames[key], candidate) {
		return
	}
	doc, err := wal.DecodeImage(frames[key])
	if err != nil {
		return
	}
	source, err := wal.DecodeImage(candidate)
	if err != nil {
		return
	}
	// The selected frame already has clean bytes. Recover original order
	// only when the other image can be used verbatim.
	if image, ok := matchingEditImage(doc, source, role, entry); ok {
		frames[key] = bytes.Clone(image)
	}
}
