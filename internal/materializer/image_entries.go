package materializer

import (
	"errors"

	"github.com/argon-lab/argon/v2/internal/wal"
)

// DocumentImageEntries returns the latest visible entries in [fromLSN, toLSN] with stored
// typed keys. A snapshot can reconstruct values but cannot attest to BSON
// field order, so keys whose entries may be missing are left out.
func (s *Service) DocumentImageEntries(branch *wal.Branch, collection string, keys []string, fromLSN, toLSN int64) (map[string]*wal.Entry, error) {
	segments, err := s.ancestrySegments(branch, toLSN)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]*wal.Entry, len(keys))
	for i := len(segments) - 1; i >= 0 && len(keys) > 0; i-- {
		seg := segments[i]
		if seg.toLSN < fromLSN {
			break
		}
		seg.fromLSN = max(fromLSN, seg.fromLSN)
		visible := func(lsn int64) bool { return !seg.branch.IsDiscardedForRead(lsn, seg.toLSN) }
		latest, err := s.wal.LatestDocumentEntries(seg.branch.ID, collection, keys, seg.fromLSN, seg.toLSN, visible)
		mixed := errors.Is(err, wal.ErrLegacyDocumentKeys)
		if mixed {
			latest, err = s.mixedImageEntries(seg, collection, keys, visible)
		}
		if err != nil {
			return nil, err
		}
		// The floor is published before deletion. A surviving older image
		// cannot prove which bytes were last, even if its values match.
		floor, err := s.DocumentImageReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return nil, err
		}
		remaining := make([]string, 0, len(keys))
		for _, key := range keys {
			if entry := latest[key]; entry != nil {
				if entry.LSN > floor {
					entries[key] = entry
				}
			} else if !mixed && !needsReclaimed(seg, seg.fromLSN, floor) {
				// No entry here: this document is inherited.
				remaining = append(remaining, key)
			}
		}
		keys = remaining
	}
	return entries, nil
}

// DocumentImageReclaimFloor includes reclamation performed before GC recorded floors.
// Only WAL images above this LSN can establish the collection's latest BSON bytes.
func (s *Service) DocumentImageReclaimFloor(branchID, collection string) (int64, error) {
	floor, err := s.wal.ReclaimFloor(branchID, collection)
	if err != nil || s.snapshots == nil {
		return floor, err
	}
	bound, err := s.snapshots.UnrecordedReclaimBound(branchID, collection)
	return max(floor, bound), err
}

func (s *Service) mixedImageEntries(seg segment, collection string, keys []string, visible func(int64) bool) (map[string]*wal.Entry, error) {
	entries := make(map[string]*wal.Entry)
	for _, key := range keys {
		entry, err := s.wal.LatestDocumentImageEntry(seg.branch.ID, collection, key, seg.fromLSN, seg.toLSN, visible)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries[key] = entry
		}
	}
	return entries, nil
}
