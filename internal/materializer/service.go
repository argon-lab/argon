// Package materializer reconstructs branch state by replaying WAL entries.
//
// Replay is deterministic by construction: every data entry is a physical
// record (a put carrying the full post-image of one document, or a delete of
// one document ID), so applying a WAL prefix is a pure fold — no filters or
// update operators are re-executed, and replaying the same prefix any number
// of times yields byte-identical state.
package materializer

import (
	"fmt"

	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// BranchLookup resolves branch metadata during ancestry traversal.
type BranchLookup interface {
	GetBranchByIDAny(branchID string) (*wal.Branch, error)
}

// Service materializes state from WAL entries
type Service struct {
	wal      *wal.Service
	branches BranchLookup
}

// NewService creates a new materializer service
func NewService(walService *wal.Service, branches BranchLookup) *Service {
	return &Service{
		wal:      walService,
		branches: branches,
	}
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
// targetLSN, following the branch's ancestry chain.
func (s *Service) MaterializeCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error) {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	state := make(map[string]bson.M)
	for _, seg := range segments {
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

	return state, nil
}

// MaterializeCollection builds the current state of a collection for a branch
func (s *Service) MaterializeCollection(branch *wal.Branch, collection string) (map[string]bson.M, error) {
	return s.MaterializeCollectionAtLSN(branch, collection, branch.HeadLSN)
}

// MaterializeBranchAtLSN builds the state of every collection in a branch as
// of targetLSN, keyed by collection name.
func (s *Service) MaterializeBranchAtLSN(branch *wal.Branch, targetLSN int64) (map[string]map[string]bson.M, error) {
	segments, err := s.ancestrySegments(branch, targetLSN)
	if err != nil {
		return nil, err
	}

	state := make(map[string]map[string]bson.M)
	for _, seg := range segments {
		entries, err := s.wal.GetBranchEntries(seg.branch.ID, "", seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get entries for branch %s: %w", seg.branch.ID, err)
		}
		for _, entry := range entries {
			if entry.Collection == "" {
				continue // Control operations carry no collection state.
			}
			if seg.branch.IsDiscardedForRead(entry.LSN, seg.toLSN) {
				continue
			}
			if _, exists := state[entry.Collection]; !exists {
				state[entry.Collection] = make(map[string]bson.M)
			}
			if err := s.ApplyEntry(state[entry.Collection], entry); err != nil {
				return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
			}
		}
	}

	return state, nil
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

	state := make(map[string]bson.M)
	for _, seg := range segments {
		entries, err := s.wal.GetDocumentHistory(seg.branch.ID, collection, documentID, seg.fromLSN, seg.toLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get document history for branch %s: %w", seg.branch.ID, err)
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

	if doc, exists := state[documentID]; exists {
		return doc, nil
	}
	return nil, nil // Document doesn't exist or was deleted.
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
		state[entry.DocumentID] = doc
	case wal.OpDelete:
		if entry.DocumentID == "" {
			return fmt.Errorf("delete entry LSN %d has no document ID", entry.LSN)
		}
		delete(state, entry.DocumentID)
	default:
		// Control operations don't affect collection state.
	}
	return nil
}
