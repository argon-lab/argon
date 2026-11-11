// Package materializer reconstructs branch state by replaying WAL entries.
//
// Replay is deterministic by construction: every data entry is a physical
// record (a put carrying the full post-image of one document, or a delete of
// one document ID), so applying a WAL prefix is a pure fold — no filters or
// update operators are re-executed, and replaying the same prefix any number
// of times yields byte-identical state.
package materializer

import (
	"errors"
	"fmt"
	"sort"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// BranchLookup resolves branch metadata during ancestry traversal.
type BranchLookup interface {
	GetBranchByIDAny(branchID string) (*wal.Branch, error)
}

// SnapshotSource serves materialized snapshots so replay can start from the
// nearest one instead of the branch root. Implemented by the snapshot
// service; nil means "always replay in full".
type SnapshotSource interface {
	// FindUsable returns the loaded state of the newest usable snapshot of
	// (branch, collection) whose LSN lies in [minLSN, maxLSN], honoring
	// discarded-range visibility for a read whose segment upper bound is
	// readUpperBound. ok is false when no usable snapshot exists.
	FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (state map[string]bson.M, lsn int64, ok bool, err error)
	// UsableLSN reports the LSN FindUsable would start from, without
	// loading the snapshot.
	UsableLSN(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (lsn int64, ok bool, err error)
	// CollectionsUpTo lists collections with snapshots for the branch in
	// the LSN window.
	CollectionsUpTo(branchID string, minLSN, maxLSN int64) ([]string, error)
}

// Service materializes state from WAL entries
type Service struct {
	wal       *wal.Service
	branches  BranchLookup
	snapshots SnapshotSource
}

// NewService creates a new materializer service
func NewService(walService *wal.Service, branches BranchLookup) *Service {
	return &Service{
		wal:      walService,
		branches: branches,
	}
}

// SetSnapshotSource wires in a snapshot provider. A setter rather than a
// constructor argument because the snapshot service itself needs the
// materializer to build snapshots — the two reference each other.
func (s *Service) SetSnapshotSource(src SnapshotSource) {
	s.snapshots = src
}

// segment is one ancestor's contribution to a branch's history: the
// ancestor's own entries in [fromLSN, toLSN], minus its discarded ranges.
type segment struct {
	branch  *wal.Branch
	fromLSN int64
	toLSN   int64
}

// maxAncestryDepth guards against corrupted parent pointers forming a cycle.
const maxAncestryDepth = 10000

// ancestrySegments returns the WAL segments whose in-order concatenation
// covers the state of a branch at targetLSN. Segments are returned
// root-first. Each hop contributes only its own entries: a branch's entries
// live in (BaseLSN, HeadLSN]; everything at or below BaseLSN is inherited
// from the parent chain. Sibling branches never appear in each other's
// chains, which is what isolates them.
func (s *Service) ancestrySegments(branch *wal.Branch, targetLSN int64) ([]segment, error) {
	var reversed []segment

	cur := branch
	limit := targetLSN
	for depth := 0; ; depth++ {
		if depth >= maxAncestryDepth {
			return nil, fmt.Errorf("branch %s ancestry exceeds %d hops: parent chain is corrupt or cyclic", branch.ID, maxAncestryDepth)
		}

		if limit > cur.BaseLSN {
			reversed = append(reversed, segment{branch: cur, fromLSN: cur.BaseLSN + 1, toLSN: limit})
		}

		if cur.ParentID == "" {
			break
		}
		parent, err := s.branches.GetBranchByIDAny(cur.ParentID)
		if err != nil {
			return nil, fmt.Errorf("branch %s references parent %s which cannot be loaded: %w", cur.ID, cur.ParentID, err)
		}

		// Entries at or below the fork point belong to the parent chain.
		// Keeping the smaller of the two matters when a branch was forked
		// from a point below its parent's own base: the effective limit
		// must not grow back up to the parent's base.
		if cur.BaseLSN < limit {
			limit = cur.BaseLSN
		}
		cur = parent
	}

	// Reverse to root-first order.
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

// MaterializeCollectionAtLSN builds the state of one collection as of
// targetLSN, following the branch's ancestry chain. When a snapshot source
// is wired in, replay starts from the nearest usable snapshot (searching
// leaf-most hop first, since a leaf snapshot covers the entire inherited
// chain beneath it) and only the delta above it is replayed.
//
// A read that would have to replay entries garbage collection has deleted
// fails with wal.ErrHistoryReclaimed instead of returning a state that
// lacks them.
func (s *Service) MaterializeCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error) {
	// A GC run that finishes between choosing the snapshot and reading the
	// entries above it invalidates that choice. One retry picks the newer
	// snapshot GC relied on; a second failure is a genuine hole.
	state, err := s.materializeCollection(branch, collection, targetLSN)
	if errors.Is(err, wal.ErrHistoryReclaimed) {
		state, err = s.materializeCollection(branch, collection, targetLSN)
	}
	return state, err
}

func (s *Service) materializeCollection(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error) {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	state := make(map[string]bson.M)
	startIdx := 0
	if s.snapshots != nil {
		for i := len(segments) - 1; i >= 0; i-- {
			seg := segments[i]
			snapState, snapLSN, ok, err := s.snapshots.FindUsable(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
			if err != nil {
				return nil, fmt.Errorf("failed to look up snapshot for branch %s: %w", seg.branch.ID, err)
			}
			if ok {
				// A snapshot's state already covers everything at or below
				// its LSN, including the inherited chain — replay resumes
				// right above it within this hop.
				state = snapState
				startIdx = i
				segments[i].fromLSN = snapLSN + 1
				break
			}
		}
	}

	for _, seg := range segments[startIdx:] {
		if seg.fromLSN > seg.toLSN {
			continue // Snapshot sits exactly at the segment's end.
		}
		entries, err := s.wal.GetBranchEntries(seg.branch.ID, collection, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get entries for branch %s: %w", seg.branch.ID, err)
		}
		for _, entry := range entries {
			if seg.branch.IsDiscardedForRead(entry.LSN, seg.toLSN) {
				continue
			}
			if err := s.ApplyEntry(state, entry); err != nil {
				return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
			}
		}
	}

	// Checked after the entries were read, not before: GC records a floor
	// before it deletes, so a floor that is clear now proves that nothing
	// in the ranges just read had been deleted yet.
	if err := s.requireUnreclaimed(segments[startIdx:], collection, targetLSN); err != nil {
		return nil, err
	}
	return state, nil
}

// requireUnreclaimed verifies that garbage collection has not deleted
// entries from the ranges a read replays. Each segment's fromLSN is where
// its replay starts — right above a snapshot, or the segment's first LSN.
func (s *Service) requireUnreclaimed(replayed []segment, collection string, targetLSN int64) error {
	for _, seg := range replayed {
		if seg.fromLSN > seg.toLSN {
			continue
		}
		floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return err
		}
		if needsReclaimed(seg, seg.fromLSN, floor) {
			return reclaimedError(seg.branch, collection, floor, targetLSN)
		}
	}
	return nil
}

// needsReclaimed reports whether a reader of the segment that replays from
// the given LSN depends on entries at or below the reclaim floor. It does
// not when the replay starts above the floor — or when every LSN from the
// start up to the floor lies in discarded ranges this reader skips anyway:
// reclaimed entries of abandoned history are no loss to a reader that
// would not have applied them.
func needsReclaimed(seg segment, from, floor int64) bool {
	upTo := floor
	if seg.toLSN < upTo {
		upTo = seg.toLSN
	}
	if upTo < from {
		return false
	}
	var skipped []wal.LSNRange
	for _, r := range seg.branch.EffectiveDiscardedRanges() {
		if seg.toLSN > r.To {
			skipped = append(skipped, r)
		}
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].From < skipped[j].From })
	next := from // lowest LSN not yet known to be skipped
	for _, r := range skipped {
		if r.To < next {
			continue
		}
		if r.From > next {
			return true
		}
		next = r.To + 1
		if next > upTo {
			return false
		}
	}
	return true
}

func reclaimedError(branch *wal.Branch, collection string, floor, targetLSN int64) error {
	return fmt.Errorf("%w: entries of collection %q on branch %q at or below LSN %d were deleted, and no snapshot usable at LSN %d covers them",
		wal.ErrHistoryReclaimed, collection, branch.Name, floor, targetLSN)
}

// ValidateReadable reports whether the branch's state at targetLSN can
// still be reconstructed, without materializing it: for every collection
// with reclaimed history it locates where replay would start and checks
// that nothing above that point is gone. Operations that would commit a
// branch to a historical position — a reset, a fork from history, a pin —
// call it first, so they are refused instead of producing a branch whose
// state silently lacks the reclaimed writes.
func (s *Service) ValidateReadable(branch *wal.Branch, targetLSN int64) error {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return nil // Before the first entry of the whole chain: the empty state.
	}
	floors := make([]map[string]int64, len(segments))
	collections := make(map[string]bool)
	for i, seg := range segments {
		if floors[i], err = s.wal.ReclaimFloors(seg.branch.ID); err != nil {
			return err
		}
		for name := range floors[i] {
			collections[name] = true
		}
	}
	for collection := range collections {
		startIdx, startLSN := 0, segments[0].fromLSN
		if s.snapshots != nil {
			for i := len(segments) - 1; i >= 0; i-- {
				seg := segments[i]
				snapLSN, ok, err := s.snapshots.UsableLSN(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
				if err != nil {
					return fmt.Errorf("failed to look up snapshot for branch %s: %w", seg.branch.ID, err)
				}
				if ok {
					startIdx, startLSN = i, snapLSN+1
					break
				}
			}
		}
		for i := startIdx; i < len(segments); i++ {
			from := segments[i].fromLSN
			if i == startIdx {
				from = startLSN
			}
			if from > segments[i].toLSN {
				continue
			}
			if floor := floors[i][collection]; needsReclaimed(segments[i], from, floor) {
				return reclaimedError(segments[i].branch, collection, floor, targetLSN)
			}
		}
	}
	return nil
}

// MaterializeCollection builds the current state of a collection for a branch
func (s *Service) MaterializeCollection(branch *wal.Branch, collection string) (map[string]bson.M, error) {
	return s.MaterializeCollectionAtLSN(branch, collection, branch.HeadLSN)
}

// MaterializeBranchAtLSN builds the state of every collection in a branch as
// of targetLSN, keyed by collection name. Collections are discovered across
// the ancestry chain (entries and snapshots) and each is materialized
// through the snapshot-aware single-collection path.
func (s *Service) MaterializeBranchAtLSN(branch *wal.Branch, targetLSN int64) (map[string]map[string]bson.M, error) {
	names, err := s.CollectionsAtLSN(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	state := make(map[string]map[string]bson.M, len(names))
	for _, name := range names {
		collState, err := s.MaterializeCollectionAtLSN(branch, name, targetLSN)
		if err != nil {
			return nil, err
		}
		state[name] = collState
	}

	return state, nil
}

// CollectionsAtLSN lists, in sorted order, the collections that can hold
// documents in a branch as of targetLSN: every collection with entries,
// snapshots or reclaimed history anywhere along the ancestry chain. Callers
// that do not need every collection in memory at once (checkout) iterate
// this and materialize one collection at a time.
func (s *Service) CollectionsAtLSN(branch *wal.Branch, targetLSN int64) ([]string, error) {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	collections := make(map[string]bool)
	for _, seg := range segments {
		names, err := s.wal.DistinctCollections(seg.branch.ID, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to list collections for branch %s: %w", seg.branch.ID, err)
		}
		for _, name := range names {
			collections[name] = true
		}
		// A collection whose entries were all reclaimed in this segment no
		// longer shows up in the WAL. It must still be read — and refused,
		// if no snapshot covers it — rather than silently left out.
		reclaimed, err := s.wal.ReclaimFloors(seg.branch.ID)
		if err != nil {
			return nil, err
		}
		for name := range reclaimed {
			collections[name] = true
		}
		if s.snapshots != nil {
			snapNames, err := s.snapshots.CollectionsUpTo(seg.branch.ID, seg.fromLSN, seg.toLSN)
			if err != nil {
				return nil, fmt.Errorf("failed to list snapshot collections for branch %s: %w", seg.branch.ID, err)
			}
			for _, name := range snapNames {
				collections[name] = true
			}
		}
	}

	names := make([]string, 0, len(collections))
	for name := range collections {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// MaterializeBranch builds the complete current state of all collections in
// a branch.
func (s *Service) MaterializeBranch(branch *wal.Branch) (map[string]map[string]bson.M, error) {
	return s.MaterializeBranchAtLSN(branch, branch.HeadLSN)
}

// MaterializeDocumentAtLSN reconstructs one document as of targetLSN, or nil
// if it does not exist at that point. This is a point lookup: it reads only
// the document's own history via the (branch, collection, document, lsn)
// index instead of replaying the whole collection.
func (s *Service) MaterializeDocumentAtLSN(branch *wal.Branch, collection, documentID string, targetLSN int64) (bson.M, error) {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	// Every physical event is a full document state, so the last visible
	// event on the nearest ancestry segment answers a point read directly.
	// Consult that segment's snapshot only when its document history has
	// been reclaimed; this avoids loading an entire snapshot on every write.
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		entries, err := s.wal.GetDocumentHistory(seg.branch.ID, collection, documentID, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get document history for branch %s: %w", seg.branch.ID, err)
		}
		for j := len(entries) - 1; j >= 0; j-- {
			entry := entries[j]
			if !entry.IsData() || seg.branch.IsDiscardedForRead(entry.LSN, seg.toLSN) {
				continue
			}
			state := make(map[string]bson.M)
			if err := s.ApplyEntry(state, entry); err != nil {
				return nil, err
			}
			return state[documentID], nil
		}
		// No entry of this segment answers, so the answer lies below: in
		// this segment's snapshot, or further down the chain. That is only
		// sound if the document really has no later entry here. Had garbage
		// collection deleted entries between that base and the read bound,
		// one of them might have been this document's — and an older value
		// would be returned as if it were current.
		floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return nil, err
		}
		if s.snapshots != nil {
			state, snapLSN, ok, err := s.snapshots.FindUsable(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
			if err != nil {
				return nil, err
			}
			if ok {
				if needsReclaimed(seg, snapLSN+1, floor) {
					return nil, reclaimedError(seg.branch, collection, floor, targetLSN)
				}
				return state[documentID], nil
			}
		}
		if needsReclaimed(seg, seg.fromLSN, floor) {
			return nil, reclaimedError(seg.branch, collection, floor, targetLSN)
		}
	}
	return nil, nil
}

// MaterializeDocument gets the current state of a specific document.
func (s *Service) MaterializeDocument(branch *wal.Branch, collection, documentID string) (bson.M, error) {
	return s.MaterializeDocumentAtLSN(branch, collection, documentID, branch.HeadLSN)
}

// ApplyEntry applies a WAL entry to a state map. Puts and deletes are
// idempotent by construction; control operations are no-ops here.
func (s *Service) ApplyEntry(state map[string]bson.M, entry *wal.Entry) error {
	if entry.IsLegacy() {
		return fmt.Errorf("entry LSN %d uses the legacy schema-v1 format (%s) and cannot be replayed deterministically; run the WAL migration", entry.LSN, entry.Operation)
	}

	switch entry.Operation {
	case wal.OpPut:
		if entry.DocumentID == "" {
			return fmt.Errorf("put entry LSN %d has no document ID", entry.LSN)
		}
		var doc bson.M
		if err := bson.Unmarshal(entry.PostImage, &doc); err != nil {
			return fmt.Errorf("failed to unmarshal post-image: %w", err)
		}
		id, err := wal.DocumentIDFromImage(entry.PostImage)
		if err != nil {
			// Early physical WAL producers kept the ID only in the envelope.
			state[entry.DocumentID] = doc
			return nil
		}
		doc["_id"] = id
		state[wal.DocumentIDString(id)] = doc
	case wal.OpDelete:
		if entry.DocumentID == "" {
			return fmt.Errorf("delete entry LSN %d has no document ID", entry.LSN)
		}
		if len(entry.PreImage) > 0 {
			id, err := wal.DocumentIDFromImage(entry.PreImage)
			if err != nil {
				delete(state, entry.DocumentID)
				return nil
			}
			delete(state, wal.DocumentIDString(id))
		} else if entry.DocumentKeyVersion > 0 {
			delete(state, entry.DocumentID)
		} else {
			// Old keys can describe two distinct BSON IDs. Resolve from the
			// existing state only when unambiguous; never guess and delete both.
			matched := ""
			for key, doc := range state {
				if wal.LegacyDocumentIDString(doc["_id"]) == entry.DocumentID {
					if matched != "" {
						return fmt.Errorf("legacy delete %q has an ambiguous BSON ID and no pre-image", entry.DocumentID)
					}
					matched = key
				}
			}
			delete(state, matched)
		}
	default:
		// Control operations don't affect collection state.
	}
	return nil
}
