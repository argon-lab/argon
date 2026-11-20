// Package materializer reconstructs branch state by replaying WAL entries.
//
// Replay is deterministic by construction: every data entry is a physical
// record (a put carrying the full post-image of one document, or a delete of
// one document ID), so applying a WAL prefix is a pure fold — no filters or
// update operators are re-executed, and replaying the same prefix any number
// of times yields byte-identical state.
package materializer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// BranchLookup resolves branch metadata during ancestry traversal.
type BranchLookup interface {
	GetBranchByIDAny(branchID string) (*wal.Branch, error)
	GetBranchByIDAnyContext(ctx context.Context, branchID string) (*wal.Branch, error)
}

// SnapshotSource serves materialized snapshots so replay can start from the
// nearest one instead of the branch root. Implemented by the snapshot
// service; nil means "always replay in full".
type SnapshotSource interface {
	// FindUsable returns the loaded state of the newest usable snapshot of
	// (branch, collection) whose LSN lies in [minLSN, maxLSN], honoring
	// discarded-range visibility for a read whose segment upper bound is
	// readUpperBound. ok is false when no usable snapshot exists.
	// The returned state belongs to the caller, which applies later WAL
	// puts and deletes to it in place.
	FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (state map[string]bson.M, lsn int64, ok bool, err error)
	// RetainedRowSnapshot reads a document from a covering ordered manifest.
	RetainedRowSnapshot(branch *wal.Branch, collection, key string, from, to int64) (bson.M, bool, error)
	// UsableLSN reports the LSN FindUsable would start from without
	// materializing its documents.
	UsableLSN(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (lsn int64, ok bool, err error)
	CheckReplayCoverage(branch *wal.Branch, collection string, from, to int64) error
	// CollectionsUpTo lists collections with snapshots for the branch in
	// the LSN window.
	CollectionsUpTo(branchID string, minLSN, maxLSN int64) ([]string, error)
	// UnrecordedReclaimBound returns the highest LSN at or below which the
	// branch's entries of a collection may have been deleted by a release
	// that recorded no reclaim floor, or 0 when that cannot have happened.
	// An empty collection name asks about every collection of the branch.
	UnrecordedReclaimBound(branchID, collection string) (int64, error)
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
	return s.collectionSegments(branch, "", targetLSN)
}

// A completed source copy supplies its collection's entire starting state.
// Stop at that copy while walking the ordinary ancestry chain.
func (s *Service) collectionSegments(branch *wal.Branch, collection string, targetLSN int64) ([]segment, error) {
	return collectionSegments(branch, collection, targetLSN, s.branches.GetBranchByIDAny)
}

func collectionSegments(branch *wal.Branch, collection string, targetLSN int64, getBranch func(string) (*wal.Branch, error)) ([]segment, error) {
	var reversed []segment

	cur := branch
	limit := targetLSN
	for depth := 0; ; depth++ {
		if depth >= maxAncestryDepth {
			return nil, fmt.Errorf("branch %s ancestry exceeds %d hops: parent chain is corrupt or cyclic", branch.ID, maxAncestryDepth)
		}

		if limit > cur.BaseLSN {
			seg := segment{branch: cur, fromLSN: cur.BaseLSN + 1, toLSN: limit}
			if len(cur.CollectionBaselines) != 0 {
				if from := cur.CollectionStart(collection, limit); from != 0 {
					seg.fromLSN = max(seg.fromLSN, from)
					reversed = append(reversed, seg)
					break
				}
			}
			reversed = append(reversed, seg)
		}

		if cur.ParentID == "" {
			break
		}
		parent, err := getBranch(cur.ParentID)
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

// CheckRejectedSnapshotCoverage checks the WAL only when a snapshot with
// uncertain old-binary keys was skipped. GC before 2.1.6 could remove its
// covered entries without recording a floor.
func (s *Service) CheckRejectedSnapshotCoverage(branch *wal.Branch, collection string, from, at, rejectedLSN, unrecordedLSN int64) error {
	floor, err := s.wal.ReclaimFloor(branch.ID, collection)
	if err != nil {
		return err
	}
	end := min(rejectedLSN, max(floor, unrecordedLSN))
	if end == 0 || !needsReclaimed(segment{branch: branch, toLSN: at}, from, end) {
		return nil
	}
	if err := s.wal.CheckOldBinaryDeletes(branch, collection, from, at); err != nil {
		return err
	}
	return &wal.ReclaimedOldBinaryError{BranchID: branch.ID, Collection: collection, From: from, To: end}
}

// CheckResyncCoverage checks a possible pre-floor GC gap identified while
// comparing the source. Certified manifests require only their metadata.
func (s *Service) CheckResyncCoverage(branch *wal.Branch, collection string) error {
	if s.snapshots == nil {
		return nil
	}
	from := max(branch.BaseLSN+1, branch.CollectionStart(collection, branch.HeadLSN))
	return s.snapshots.CheckReplayCoverage(branch, collection, from, branch.HeadLSN)
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
	return replayCollection(s, branch, collection, targetLSN, s.decodedSnapshot, decodedValue)
}

func (s *Service) decodedSnapshot(branch *wal.Branch, collection string, from, to int64) (map[string]bson.M, int64, bool, error) {
	return s.snapshots.FindUsable(branch, collection, from, to, to)
}

// A frame visitor lends the decoded value and stored bytes of the same frame.
type frameSource interface {
	VisitUsableFrames(branch *wal.Branch, collection string, from, to int64, reset func(), visit func(string, *wal.DecodedImage) error) (lsn int64, found, ordered bool, err error)
}

// Deliver the requested key only after the entire snapshot has loaded.
func (s *Service) visitSnapshot(branch *wal.Branch, collection string, from, to int64, key *string, visit func(string, *wal.DecodedImage) error) (int64, bool, bool, error) {
	if source, ok := s.snapshots.(frameSource); ok {
		var match *wal.DecodedImage
		lsn, found, ordered, err := source.VisitUsableFrames(branch, collection, from, to, func() { match = nil }, func(k string, doc *wal.DecodedImage) error {
			if k == *key {
				match = doc
			}
			return nil
		})
		if err == nil && match != nil {
			err = visit(*key, match)
		}
		return lsn, found, ordered, err
	}
	docs, lsn, found, err := s.decodedSnapshot(branch, collection, from, to)
	if doc := docs[*key]; err == nil && found && doc != nil {
		err = visit(*key, wal.DecodedValue(doc))
	}
	return lsn, found, false, err
}

// Decoded-only providers can release each document as its encoded form takes
// its place. The snapshot service streams both directly from the same frame.
func (s *Service) visitSnapshotFrames(branch *wal.Branch, collection string, from, to int64, reset func(), visit func(string, *wal.DecodedImage) error) (int64, bool, bool, error) {
	if source, ok := s.snapshots.(frameSource); ok {
		return source.VisitUsableFrames(branch, collection, from, to, reset, visit)
	}
	docs, lsn, found, err := s.decodedSnapshot(branch, collection, from, to)
	if err != nil || !found {
		return lsn, found, false, err
	}
	for k, doc := range docs {
		if err := visit(k, wal.DecodedValue(doc)); err != nil {
			return 0, false, false, err
		}
		delete(docs, k)
	}
	return lsn, true, false, nil
}

func replayCollection[T any](s *Service, branch *wal.Branch, collection string, targetLSN int64,
	snapshot func(*wal.Branch, string, int64, int64) (map[string]T, int64, bool, error),
	value replayValue[T],
) (map[string]T, error) {
	// If GC overtook the read, retry once with the snapshot GC relied on.
	state, err := replayCollectionOnce(s, branch, collection, targetLSN, snapshot, value)
	if errors.Is(err, wal.ErrHistoryReclaimed) {
		return replayCollectionOnce(s, branch, collection, targetLSN, snapshot, value)
	}
	return state, err
}

func replayCollectionOnce[T any](s *Service, branch *wal.Branch, collection string, targetLSN int64,
	snapshot func(*wal.Branch, string, int64, int64) (map[string]T, int64, bool, error),
	value replayValue[T],
) (map[string]T, error) {
	segments, err := s.collectionSegments(branch, collection, targetLSN)
	if err != nil {
		return nil, err
	}

	state := make(map[string]T)
	startIdx := 0
	if s.snapshots != nil {
		for i := len(segments) - 1; i >= 0; i-- {
			seg := segments[i]
			snapState, snapLSN, ok, err := snapshot(seg.branch, collection, seg.fromLSN, seg.toLSN)
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
		if err := scanSegment(s, collection, state, seg, value); err != nil {
			return nil, fmt.Errorf("failed to replay collection %s on branch %s: %w", collection, seg.branch.ID, err)
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

func scanSegment[T any](s *Service, collection string, state map[string]T, seg segment, value replayValue[T]) error {
	return s.wal.ScanCollectionDataEntries(seg.branch.ID, collection, seg.fromLSN, seg.toLSN, false, func(entry *wal.Entry) error {
		if seg.branch.IsDiscardedForRead(entry.LSN, seg.toLSN) {
			return nil
		}
		if err := applyEntry(state, entry, value); err != nil {
			return fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
		return nil
	})
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
	if min(seg.toLSN, floor) < from {
		return false
	}
	return lastReclaimableLSN(seg, floor) >= from
}

// The latest visible slot that GC could have removed. Effective range ends
// increase in reset order, so walking backwards also handles overlapping resets.
func lastReclaimableLSN(seg segment, floor int64) int64 {
	last := min(seg.toLSN, floor)
	ranges := seg.branch.EffectiveDiscardedRanges()
	for i := len(ranges) - 1; i >= 0; i-- {
		r := ranges[i]
		if last > r.To {
			break
		}
		if seg.toLSN > r.To && r.Contains(last) {
			last = r.From - 1
		}
	}
	return last
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
		for i := len(segments) - 1; i >= 0; i-- {
			seg := segments[i]
			baseline := seg.branch.CollectionStart(collection, seg.toLSN)
			if s.snapshots != nil {
				snapLSN, ok, err := s.snapshots.UsableLSN(seg.branch, collection, max(seg.fromLSN, baseline), seg.toLSN, seg.toLSN)
				if err != nil {
					return fmt.Errorf("failed to look up snapshot for branch %s: %w", seg.branch.ID, err)
				}
				if ok {
					startIdx, startLSN = i, snapLSN+1
					break
				}
			}
			if baseline != 0 {
				startIdx, startLSN = i, max(seg.fromLSN, baseline)
				break
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
				if s.snapshots != nil {
					// A descendant's rejected manifest can identify why
					// this ancestral range cannot be reconstructed.
					for j := len(segments) - 1; j >= i; j-- {
						seg := segments[j]
						start := max(seg.fromLSN, seg.branch.CollectionStart(collection, seg.toLSN))
						if err := s.snapshots.CheckReplayCoverage(seg.branch, collection, start, seg.toLSN); err != nil {
							return err
						}
					}
				}
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
	segments, err := s.collectionSegments(branch, collection, targetLSN)
	if err != nil {
		return nil, err
	}

	// Every physical event is a full document state, so the last visible
	// event on the nearest ancestry segment answers a point read directly.
	// Consult that segment's snapshot only when its document history has
	// been reclaimed; this avoids loading an entire snapshot on every write.
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		entries, collision, err := s.wal.DocumentHistoryForRead(seg.branch.ID, collection, documentID, seg.fromLSN, seg.toLSN, func(lsn int64) bool { return !seg.branch.IsDiscardedForRead(lsn, seg.toLSN) })
		if err != nil {
			return nil, fmt.Errorf("failed to get document history for branch %s: %w", seg.branch.ID, err)
		}
		var latest *wal.Entry
		for j := len(entries) - 1; j >= 0; j-- {
			entry := entries[j]
			if entry.IsData() && !seg.branch.IsDiscardedForRead(entry.LSN, seg.toLSN) {
				latest = entry
				break
			}
		}
		var floor int64
		if collision && latest != nil {
			// A newer ambiguous delete may already have been reclaimed,
			// while this earlier image still awaits deletion.
			floor, err = s.DocumentImageReclaimFloor(seg.branch.ID, collection)
		} else if latest == nil {
			floor, err = s.wal.ReclaimFloor(seg.branch.ID, collection)
		}
		if err != nil {
			return nil, err
		}
		if latest != nil && (!collision || latest.LSN > floor) {
			state := make(map[string]bson.M)
			if err := s.ApplyEntry(state, latest); err != nil {
				return nil, err
			}
			if !collision && seg.branch.ReclaimGate(collection) >= latest.LSN {
				floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
				if err != nil {
					return nil, err
				}
				if latest.LSN <= floor && s.snapshots != nil {
					from := max(latest.LSN, lastReclaimableLSN(seg, floor))
					doc, ok, err := s.snapshots.RetainedRowSnapshot(seg.branch, collection, documentID, from, seg.toLSN)
					if err != nil {
						return nil, err
					}
					if ok {
						// The snapshot includes the entire prefix. A missing key
						// there supersedes the retained row just as a newer put does.
						return doc, nil
					}
				}
			}
			return state[documentID], nil
		}
		// No entry of this segment answers, so the answer lies below: in
		// this segment's snapshot, or further down the chain. That is only
		// sound if the document really has no later entry here. Had garbage
		// collection deleted entries between that base and the read bound,
		// one of them might have been this document's — and an older value
		// would be returned as if it were current.
		if s.snapshots != nil {
			doc, snapLSN, ok, err := s.snapshotDocument(seg, collection, documentID)
			if err != nil {
				return nil, err
			}
			if ok {
				if needsReclaimed(seg, snapLSN+1, floor) {
					return nil, reclaimedError(seg.branch, collection, floor, targetLSN)
				}
				return doc, nil
			}
		}
		if needsReclaimed(seg, seg.fromLSN, floor) {
			return nil, reclaimedError(seg.branch, collection, floor, targetLSN)
		}
	}
	return nil, nil
}

// Check every snapshot frame for decoding errors and keep the requested value.
func (s *Service) snapshotDocument(seg segment, collection, key string) (bson.M, int64, bool, error) {
	var value bson.M
	lsn, ok, _, err := s.visitSnapshot(seg.branch, collection, seg.fromLSN, seg.toLSN, &key, func(_ string, doc *wal.DecodedImage) error {
		value = doc.TakeValue()
		return nil
	})
	return value, lsn, ok, err
}

// MaterializeDocumentsAtLSN reconstructs the given documents of one
// collection as of targetLSN. The result holds the documents that exist at
// that point; a key that is absent from it names a document that does not.
//
// It answers from the documents' own histories, walking the ancestry chain
// from the branch towards the root and settling each document at the first
// segment that has an entry for it. The cost therefore follows the number of
// documents asked for, not the size of the collection. A snapshot is loaded
// only where garbage collection has, or may have, deleted entries of a
// segment: there the log can no longer show that a document went unchanged,
// and the snapshot is the remaining record of its state.
//
// The result equals what MaterializeCollectionAtLSN returns for the same
// keys. Where it cannot be established — reclaimed history without a usable
// snapshot, or legacy entries whose document cannot be told from the entry
// alone — an error is returned and the caller should replay the collection.
func (s *Service) MaterializeDocumentsAtLSN(branch *wal.Branch, collection string, keys []string, targetLSN int64) (map[string]bson.M, error) {
	result := make(map[string]bson.M, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	segments, err := s.collectionSegments(branch, collection, targetLSN)
	if err != nil {
		return nil, err
	}

	remaining := make(map[string]bool, len(keys))
	for _, key := range keys {
		remaining[key] = true
	}
	pending := func() []string {
		list := make([]string, 0, len(remaining))
		for key := range remaining {
			list = append(list, key)
		}
		sort.Strings(list)
		return list
	}

	for i := len(segments) - 1; i >= 0 && len(remaining) > 0; i-- {
		seg := segments[i]
		entries, err := s.wal.LatestDocumentEntries(seg.branch.ID, collection, pending(), seg.fromLSN, seg.toLSN,
			func(lsn int64) bool { return !seg.branch.IsDiscardedForRead(lsn, seg.toLSN) })
		if err != nil {
			return nil, fmt.Errorf("failed to get document histories for branch %s: %w", seg.branch.ID, err)
		}
		// A point read finds entries by document key. An entry that names no
		// document (schema-v1 updates and deletes carried a filter instead)
		// is invisible to it, while a replay would stop at it.
		unkeyed, err := s.wal.HasUnkeyedDataEntries(seg.branch.ID, collection, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, err
		}
		if unkeyed {
			return nil, fmt.Errorf("collection %q on branch %q holds entries that name no document; replay the collection", collection, seg.branch.Name)
		}

		// Read the floor after the entries: GC records it before it deletes,
		// so entries above a floor read now were all still in the log.
		floor, err := s.wal.ReclaimFloor(seg.branch.ID, collection)
		if err != nil {
			return nil, err
		}
		for key, entry := range entries {
			if entry.LSN <= floor {
				// Deletion at or below the floor may be under way or half
				// done: a newer entry of this document can already be gone.
				// A replay never applies such an entry either; the snapshot
				// below answers for this document.
				continue
			}
			state := make(map[string]bson.M, 1)
			if err := s.ApplyEntry(state, entry); err != nil {
				return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
			}
			if doc, ok := state[key]; ok {
				result[key] = doc
			}
			delete(remaining, key)
		}
		if len(remaining) == 0 {
			break
		}

		// The documents still pending have no usable entry in this segment.
		// That shows they went unchanged here only if none of the segment's
		// entries can be missing. Two things can have removed some: garbage
		// collection that recorded a floor, and garbage collection by a
		// release that did not (up to 2.1.5), which is bounded by the
		// snapshots of that time.
		suspect := floor
		if s.snapshots != nil {
			unrecorded, err := s.snapshots.UnrecordedReclaimBound(seg.branch.ID, collection)
			if err != nil {
				return nil, fmt.Errorf("failed to look up snapshots for branch %s: %w", seg.branch.ID, err)
			}
			if unrecorded > suspect {
				suspect = unrecorded
			}
		}
		if !needsReclaimed(seg, seg.fromLSN, suspect) {
			continue
		}
		// The log cannot answer for this segment; its snapshot can, as long
		// as nothing between the snapshot and the read bound may be missing.
		// Without one, say so: the caller's collection replay may still
		// succeed from a snapshot further up the chain, and reports what is
		// lost if it cannot.
		if s.snapshots == nil {
			return nil, reclaimedError(seg.branch, collection, suspect, targetLSN)
		}
		state, snapLSN, ok, err := s.snapshots.FindUsable(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
		if err != nil && !errors.Is(err, wal.ErrHistoryReclaimed) {
			return nil, fmt.Errorf("failed to look up snapshot for branch %s: %w", seg.branch.ID, err)
		}
		if err != nil || !ok || needsReclaimed(seg, snapLSN+1, suspect) {
			if i < len(segments)-1 {
				// Point lookups above this hop skipped its descendants'
				// snapshots because their own WAL was intact. A descendant
				// snapshot can still cover this ancestral gap, or hold the
				// reason that its historical state cannot be established.
				full, err := s.MaterializeCollectionAtLSN(branch, collection, targetLSN)
				if err != nil {
					return nil, err
				}
				for key := range remaining {
					if doc, ok := full[key]; ok {
						result[key] = doc
					}
				}
				return result, nil
			}
			if err != nil {
				return nil, err
			}
			return nil, reclaimedError(seg.branch, collection, suspect, targetLSN)
		}
		// Entries above the snapshot were looked at first, so a pending
		// document is unchanged since the snapshot, which already holds the
		// state inherited from the rest of the chain.
		for key := range remaining {
			if doc, ok := state[key]; ok {
				result[key] = doc
			}
		}
		remaining = nil
	}
	return result, nil
}

// UnrecordedReclaimBound returns the highest LSN at or below which entries of
// the branch's own log, in any collection, may have been deleted without a
// reclaim floor (see SnapshotSource), or 0.
func (s *Service) UnrecordedReclaimBound(branch *wal.Branch) (int64, error) {
	if s.snapshots == nil {
		return 0, nil
	}
	return s.snapshots.UnrecordedReclaimBound(branch.ID, "")
}

// EntryStateKey returns the key under which ApplyEntry stores or removes the
// entry's document. ok is false when the entry alone does not say: an entry
// that is not a current put or delete, one without a document ID, or a
// legacy delete without a pre-image, which ApplyEntry resolves by searching
// the state it is folding into.
func EntryStateKey(entry *wal.Entry) (key string, ok bool) {
	if entry.IsLegacy() || entry.DocumentID == "" {
		return "", false
	}
	switch entry.Operation {
	case wal.OpPut:
		if id, err := wal.DocumentIDFromImage(entry.PostImage); err == nil {
			return wal.DocumentIDString(id), true
		}
		return entry.DocumentID, entry.CheckStoredKey() == nil
	case wal.OpDelete:
		if len(entry.PreImage) > 0 {
			if id, err := wal.DocumentIDFromImage(entry.PreImage); err == nil {
				return wal.DocumentIDString(id), true
			}
			return entry.DocumentID, entry.CheckStoredKey() == nil
		}
		if entry.DocumentKeyVersion > 0 && entry.CheckStoredKey() == nil {
			return entry.DocumentID, true
		}
	}
	return "", false
}

// MaterializeDocument gets the current state of a specific document.
func (s *Service) MaterializeDocument(branch *wal.Branch, collection, documentID string) (bson.M, error) {
	return s.MaterializeDocumentAtLSN(branch, collection, documentID, branch.HeadLSN)
}

// ApplyEntry applies a WAL entry to a state map. Puts and deletes are
// idempotent by construction; control operations are no-ops here.
func (s *Service) ApplyEntry(state map[string]bson.M, entry *wal.Entry) error {
	return applyEntry(state, entry, decodedValue)
}

// The replay decides each key, value and deletion before choosing how to
// store the value. The identity accessor is needed only for untyped deletes.
type replayValue[T any] struct {
	store func(*wal.DecodedImage, *wal.Entry) (T, error)
	id    func(T) any
}

var decodedValue = replayValue[bson.M]{
	store: func(doc *wal.DecodedImage, _ *wal.Entry) (bson.M, error) { return doc.TakeValue(), nil },
	id:    func(doc bson.M) any { return doc["_id"] },
}

func applyEntry[T any](state map[string]T, entry *wal.Entry, value replayValue[T]) error {
	if entry.IsLegacy() {
		return fmt.Errorf("entry LSN %d uses the legacy schema-v1 format (%s) and cannot be replayed deterministically; run the WAL migration", entry.LSN, entry.Operation)
	}

	switch entry.Operation {
	case wal.OpPut:
		if entry.DocumentID == "" {
			return fmt.Errorf("put entry LSN %d has no document ID", entry.LSN)
		}
		doc, err := wal.DecodeImage(entry.PostImage)
		key, idErr := doc.Key()
		if idErr != nil {
			if ambiguous := entry.CheckStoredKey(); ambiguous != nil {
				return errors.Join(ambiguous, idErr)
			}
			// Early physical WAL producers kept the ID only in the envelope.
			key = entry.DocumentID
		}
		if err != nil {
			return fmt.Errorf("failed to unmarshal post-image for %s/%s: %w", entry.Collection, entry.DocumentID, err)
		}
		stored, err := value.store(doc, entry)
		if err != nil {
			return err
		}
		state[key] = stored
	case wal.OpDelete:
		if entry.DocumentID == "" {
			return fmt.Errorf("delete entry LSN %d has no document ID", entry.LSN)
		}
		if len(entry.PreImage) > 0 {
			id, err := wal.DocumentIDFromImage(entry.PreImage)
			if err != nil {
				if ambiguous := entry.CheckStoredKey(); ambiguous != nil {
					return errors.Join(ambiguous, err)
				}
				delete(state, entry.DocumentID)
				return nil
			}
			delete(state, wal.DocumentIDString(id))
		} else if err := entry.CheckStoredKey(); err != nil {
			return err
		} else if entry.DocumentKeyVersion > 0 {
			delete(state, entry.DocumentID)
		} else {
			// Old keys can describe two distinct BSON IDs. Resolve from the
			// existing state only when unambiguous; never guess and delete both.
			matched := ""
			for key, doc := range state {
				if wal.LegacyDocumentIDString(value.id(doc)) == entry.DocumentID {
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
