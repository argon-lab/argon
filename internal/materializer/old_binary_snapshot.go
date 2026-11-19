package materializer

import (
	"context"
	"sort"

	"github.com/argon-lab/argon/v2/internal/wal"
)

// OldBinarySnapshotHistory describes the WAL prefix used to build one old
// snapshot. A missing interval matters only if some independent record
// establishes that this collection used old-binary IDs.
type OldBinarySnapshotHistory struct {
	Ambiguous bool
	Evidence  bool
	Reclaimed *wal.ReclaimedOldBinaryError
}

type oldBinaryCollection struct {
	branchID, collection string
}

type oldBinaryPrefix struct {
	history wal.OldBinaryHistory
	floor   int64
}

// OldBinarySnapshotPass shares ancestry, retained-key scans and reclaim
// floors across all the manifests checked together. An on-demand read
// starts a fresh pass. Neither pass retains mutable branch state afterwards.
type OldBinarySnapshotPass struct {
	service  *Service
	branches map[string]*wal.Branch
	prefixes map[oldBinaryCollection]oldBinaryPrefix
	live     map[oldBinaryCollection]bool
}

func (s *Service) NewOldBinarySnapshotPass() *OldBinarySnapshotPass {
	return &OldBinarySnapshotPass{
		service: s, branches: make(map[string]*wal.Branch),
		prefixes: make(map[oldBinaryCollection]oldBinaryPrefix), live: make(map[oldBinaryCollection]bool),
	}
}

// Branch includes deleted parents whose WAL still supplies a child's state.
func (p *OldBinarySnapshotPass) Branch(ctx context.Context, id string) (*wal.Branch, error) {
	if branch, ok := p.branches[id]; ok {
		return branch, nil
	}
	branch, err := p.service.branches.GetBranchByIDAnyContext(ctx, id)
	if err == nil {
		p.branches[id] = branch
	}
	return branch, err
}

func (p *OldBinarySnapshotPass) prefix(ctx context.Context, key oldBinaryCollection) (oldBinaryPrefix, error) {
	if seen, ok := p.prefixes[key]; ok {
		return seen, nil
	}
	history, err := p.service.wal.ScanOldBinaryHistory(ctx, key.branchID, key.collection)
	if err != nil {
		return oldBinaryPrefix{}, err
	}
	// Read after the WAL: GC records the floor before deleting entries.
	floor, err := p.service.wal.ReclaimFloorContext(ctx, key.branchID, key.collection)
	if err != nil {
		return oldBinaryPrefix{}, err
	}
	result := oldBinaryPrefix{history: history, floor: floor}
	p.prefixes[key] = result
	return result, nil
}

func (p *OldBinarySnapshotPass) Check(ctx context.Context, branch *wal.Branch, collection string, snapLSN int64) (OldBinarySnapshotHistory, error) {
	var result OldBinarySnapshotHistory
	if err := ctx.Err(); err != nil {
		return result, err
	}
	p.branches[branch.ID] = branch
	segs, err := collectionSegments(branch, "", snapLSN, func(id string) (*wal.Branch, error) { return p.Branch(ctx, id) })
	if err != nil {
		return result, err
	}
	for _, seg := range segs {
		// Old writers ignore a completed source baseline on an ancestor.
		if seg.branch.CollectionStart(collection, seg.toLSN) != 0 {
			result.Ambiguous = true
		}
		prefix, err := p.prefix(ctx, oldBinaryCollection{seg.branch.ID, collection})
		if err != nil {
			return result, err
		}
		result.Evidence = result.Evidence || prefix.history.Evidence
		deletes := prefix.history.Deletes
		start := sort.Search(len(deletes), func(i int) bool { return deletes[i].LSN >= seg.fromLSN })
		for _, del := range deletes[start:] {
			if del.LSN > seg.toLSN {
				break
			}
			if !seg.branch.IsDiscardedForRead(del.LSN, seg.toLSN) {
				result.Ambiguous = true
				break
			}
		}
		if needsReclaimed(seg, seg.fromLSN, prefix.floor) {
			// Keep the leaf-most gap. Any snapshot covering it necessarily
			// covers the older ancestry as well.
			result.Reclaimed = &wal.ReclaimedOldBinaryError{
				BranchID: seg.branch.ID, Collection: collection, From: seg.fromLSN, To: min(prefix.floor, seg.toLSN),
			}
		}
	}
	if result.Reclaimed != nil && !result.Evidence && branch.IsLive() {
		key := oldBinaryCollection{branch.ID, collection}
		if evidence, ok := p.live[key]; ok {
			result.Evidence = evidence
		} else {
			result.Evidence, err = p.service.wal.HasLiveOldBinaryID(ctx, branch, collection)
			if err != nil {
				return result, err
			}
			p.live[key] = result.Evidence
		}
	}
	return result, nil
}

// CheckAncestorSnapshotCoverage allows replay from an independent snapshot
// between the rejected manifest and its missing ancestral WAL interval.
func (s *Service) CheckAncestorSnapshotCoverage(branch *wal.Branch, collection string, at int64, gap *wal.ReclaimedOldBinaryError) error {
	segs, err := s.collectionSegments(branch, collection, at)
	if err != nil {
		return err
	}
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if seg.branch.ID == branch.ID {
			continue // The caller already tried every usable local snapshot.
		}
		lsn, ok, err := s.snapshots.UsableLSN(seg.branch, collection, seg.fromLSN, seg.toLSN, seg.toLSN)
		if err != nil {
			return err
		}
		if ok && (seg.branch.ID != gap.BranchID || !needsReclaimed(seg, lsn+1, gap.To)) {
			return nil
		}
		if seg.branch.ID == gap.BranchID {
			return gap
		}
	}
	// A completed baseline can cut off that ancestor for this read.
	return nil
}
