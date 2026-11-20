// Package gc reclaims WAL entries that no reader can ever need again.
//
// The coverage argument, per (branch, collection):
//
//   - Readers of the branch itself (at any current or future head) start
//     from the newest snapshot that is valid for them. A snapshot at LSN S
//     that is usable for *every* future reader (no later discarded range
//     overlaps it) covers all entries with LSN <= S.
//   - A live child forked at F reads this branch's segment with upper
//     bound exactly F — and so does every deeper descendant through that
//     child, because ancestry traversal clamps the limit to the fork
//     point at each hop. Those readers can only use snapshots with
//     LSN <= F, so each live child needs entries above its own coverage
//     snapshot S_i (the newest usable snapshot at or below F_i).
//   - The retention window is the time-travel guarantee: every LSN inside
//     the window must stay materializable. A read at such an LSN starts
//     from the newest snapshot at or below it, so it needs every entry
//     above that snapshot — including entries older than the window that
//     sit between the snapshot and the window's start. Reclamation
//     therefore stops at a snapshot: the newest one, valid for every
//     reader, that is at or below R, the last LSN older than the window.
//
// The reclaim cutoff is therefore
//
//	min( S_R, min over live children of S_i, min over pins of S_p )
//
// where S_R is that snapshot. Entries at or below the cutoff are deleted;
// everything else stays. With no such snapshot (S_R = 0) nothing is
// deleted, no matter how old — history that is anyone's only source of
// truth is never dropped.
//
// Cutting at R itself, rather than at a snapshot below it, would delete the
// entries between that snapshot and R while in-window reads just above R
// still replay across them — and silently come back missing those writes.
//
// Deleting entries below the cutoff also deletes discarded-range entries
// and pre-images in that region, which ends their audit/undo availability.
// That is exactly what a retention window means; pick it accordingly.
package gc

import (
	"context"
	"fmt"
	"math"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
)

// Config tunes garbage collection.
type Config struct {
	// RetentionWindow is how far back in time historical reads (time
	// travel, audit, undo) remain guaranteed. Entries younger than this
	// are never reclaimed.
	RetentionWindow time.Duration
	// DryRun reports what would be deleted without deleting it.
	DryRun bool
}

// DefaultConfig keeps one week of history.
func DefaultConfig() Config {
	return Config{RetentionWindow: 7 * 24 * time.Hour}
}

// Service reclaims WAL entries covered by snapshots and outside retention.
type Service struct {
	wal       *wal.Service
	branches  *branchwal.BranchService
	snapshots *snapshot.Service

	// pinLSNs returns the pinned LSNs on a branch. Wired by the caller so
	// this package does not depend on the pin package. A pin at LSN P is a
	// permanent reader at bound P: it clamps the cutoff exactly like a
	// live child's fork point does.
	pinLSNs func(branchID string) ([]int64, error)
}

// SetPinLookup registers the pinned-LSN lookup used to protect pinned
// history from reclamation.
func (s *Service) SetPinLookup(lookup func(branchID string) ([]int64, error)) {
	s.pinLSNs = lookup
}

// NewService creates a GC service.
func NewService(walService *wal.Service, branches *branchwal.BranchService, snapshots *snapshot.Service) *Service {
	return &Service{wal: walService, branches: branches, snapshots: snapshots}
}

// BranchReport describes what GC did (or would do) for one branch.
type BranchReport struct {
	BranchID       string
	BranchName     string
	EntriesRemoved int64
	Cutoffs        map[string]int64 // collection -> reclaim cutoff LSN
}

// Report summarizes a project GC run.
type Report struct {
	ProjectID      string
	Branches       []BranchReport
	EntriesRemoved int64
	DryRun         bool
}

// RunProject reclaims covered, out-of-retention entries across every live
// branch of a project.
func (s *Service) RunProject(ctx context.Context, projectID string, cfg Config) (*Report, error) {
	if cfg.RetentionWindow < 0 {
		return nil, fmt.Errorf("retention window must not be negative")
	}

	branches, err := s.branches.ListBranchesAny(projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	// Live children per parent: their fork points constrain the parent.
	childrenOf := make(map[string][]*wal.Branch)
	for _, b := range branches {
		if !b.IsDeleted && b.ParentID != "" {
			childrenOf[b.ParentID] = append(childrenOf[b.ParentID], b)
		}
	}

	report := &Report{ProjectID: projectID, DryRun: cfg.DryRun}
	retentionCutoffTime := time.Now().Add(-cfg.RetentionWindow)

	for _, branch := range branches {
		if branch.IsDeleted {
			continue // Reclaimed at deletion time via the delete hook.
		}
		br, err := s.gcBranch(ctx, branch, childrenOf[branch.ID], retentionCutoffTime, cfg.DryRun)
		if err != nil {
			return nil, fmt.Errorf("branch %s (%s): %w", branch.Name, branch.ID, err)
		}
		report.Branches = append(report.Branches, *br)
		report.EntriesRemoved += br.EntriesRemoved
	}
	return report, nil
}

func (s *Service) gcBranch(ctx context.Context, branch *wal.Branch, liveChildren []*wal.Branch, retentionCutoffTime time.Time, dryRun bool) (*BranchReport, error) {
	report := &BranchReport{
		BranchID:   branch.ID,
		BranchName: branch.Name,
		Cutoffs:    make(map[string]int64),
	}

	// R: the retention window translated into this branch's LSNs — the
	// last LSN below which everything is older than the window.
	retentionLSN, err := s.wal.LatestLSNBefore(branch.ID, retentionCutoffTime)
	if err != nil {
		return nil, fmt.Errorf("failed to compute retention cutoff: %w", err)
	}
	if retentionLSN == 0 {
		return report, nil // Everything is inside the window.
	}
	// Timestamps are not strictly ordered by LSN (they are taken after the
	// LSN is reserved), so an in-window entry can sit below the newest old
	// one. Nothing at or above the first in-window entry is reclaimable.
	firstInWindow, err := s.wal.FirstLSNAfter(branch.ID, retentionCutoffTime)
	if err != nil {
		return nil, fmt.Errorf("failed to compute retention cutoff: %w", err)
	}
	if firstInWindow > 0 && firstInWindow-1 < retentionLSN {
		retentionLSN = firstInWindow - 1
	}
	if retentionLSN <= 0 {
		return report, nil
	}

	collections, err := s.wal.DistinctCollections(branch.ID, 0, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}

	var pinnedLSNs []int64
	if s.pinLSNs != nil {
		pinnedLSNs, err = s.pinLSNs(branch.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to look up pins: %w", err)
		}
	}

	for _, collection := range collections {
		// S_R: the newest snapshot at or below the retention boundary that
		// is valid for every possible read bound. Readers of the branch
		// itself, at the head or anywhere inside the window, start from it
		// or from a newer snapshot, and replay only entries above it.
		cutoff, err := s.snapshots.NewestUsableLSN(branch, collection, min64(branch.HeadLSN, retentionLSN), math.MaxInt64)
		if err != nil {
			return nil, err
		}
		if cutoff == 0 {
			continue // No such snapshot: this history is the only source of truth.
		}

		// S_i: every live child's readers are pinned to its fork point and
		// can only use snapshots at or below it.
		for _, child := range liveChildren {
			childCoverage, err := s.snapshots.NewestUsableLSN(branch, collection, child.BaseLSN, child.BaseLSN)
			if err != nil {
				return nil, err
			}
			cutoff = min64(cutoff, childCoverage)
		}

		// Pins are permanent readers at their LSN: same rule as fork
		// points. No usable snapshot at or below the pin means nothing
		// here may be reclaimed at all.
		for _, pinLSN := range pinnedLSNs {
			pinCoverage, err := s.snapshots.NewestUsableLSN(branch, collection, pinLSN, pinLSN)
			if err != nil {
				return nil, err
			}
			cutoff = min64(cutoff, pinCoverage)
		}

		if cutoff <= 0 {
			continue
		}
		report.Cutoffs[collection] = cutoff

		if dryRun {
			continue
		}
		// The floor goes down first. Readers that would need the deleted
		// entries are refused from here on instead of replaying past them.
		if err := s.wal.RecordReclaimFloor(ctx, branch.ID, collection, cutoff); err != nil {
			return nil, err
		}
		removed, err := s.wal.DeleteDataEntriesUpTo(branch.ID, collection, cutoff)
		if err != nil {
			return nil, fmt.Errorf("failed to delete entries for %s: %w", collection, err)
		}
		report.EntriesRemoved += removed
	}
	return report, nil
}

// ReclaimDeletedBranch removes everything a deleted branch owned: WAL
// entries and snapshots (manifests plus unshared chunks). Only safe for
// regular deletion, which refuses branches with children — nothing can
// traverse the branch's history afterwards.
func (s *Service) ReclaimDeletedBranch(ctx context.Context, branchID string) (entriesRemoved, manifestsRemoved, chunksRemoved int64, err error) {
	entriesRemoved, err = s.wal.DeleteBranchEntries(branchID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to delete branch entries: %w", err)
	}
	manifestsRemoved, chunksRemoved, err = s.snapshots.CleanupBranch(ctx, branchID)
	if err != nil {
		return entriesRemoved, 0, 0, err
	}
	if err := s.wal.ForgetReclaimFloors(ctx, branchID); err != nil {
		return entriesRemoved, manifestsRemoved, chunksRemoved, fmt.Errorf("failed to drop reclaim floors: %w", err)
	}
	return entriesRemoved, manifestsRemoved, chunksRemoved, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
