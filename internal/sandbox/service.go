// Package sandbox implements ephemeral agent branches: fork, check out,
// hand an agent the connection string, and let the TTL clean up whatever
// wasn't merged or explicitly kept. This is the Neon-style workflow that
// makes it safe to point an AI agent at production-shaped data.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/wal"
)

// DefaultTTL bounds sandboxes that don't specify one.
const DefaultTTL = time.Hour

// Service creates and reaps sandbox branches.
type Service struct {
	branches *branchwal.BranchService
	checkout *checkout.Service
}

// NewService creates a sandbox service.
func NewService(branches *branchwal.BranchService, co *checkout.Service) *Service {
	return &Service{branches: branches, checkout: co}
}

// Info describes a created sandbox.
type Info struct {
	BranchID   string
	BranchName string
	PhysicalDB string
	ExpiresAt  time.Time
	ForkedFrom string
	ForkLSN    int64
}

// Create forks a sandbox branch from the given parent, checks it out and
// stamps the TTL. The returned physical database is what the agent
// connects to; run the ingester (argon watch) to capture its writes.
func (s *Service) Create(ctx context.Context, projectID, parentBranchID, name string, ttl time.Duration) (*Info, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if name == "" {
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return nil, fmt.Errorf("failed to generate sandbox name: %w", err)
		}
		name = "sandbox-" + hex.EncodeToString(suffix)
	}

	parent, err := s.branches.GetBranchByID(parentBranchID)
	if err != nil {
		return nil, fmt.Errorf("parent branch not found: %w", err)
	}

	branch, err := s.branches.CreateBranch(projectID, name, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to fork sandbox: %w", err)
	}

	expires := time.Now().Add(ttl)
	if err := s.branches.SetExpiry(branch.ID, &expires); err != nil {
		return nil, fmt.Errorf("failed to stamp sandbox TTL: %w", err)
	}

	info, err := s.checkout.Checkout(ctx, branch.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to check the sandbox out: %w", err)
	}

	return &Info{
		BranchID:   branch.ID,
		BranchName: branch.Name,
		PhysicalDB: info.PhysicalDB,
		ExpiresAt:  expires,
		ForkedFrom: parent.Name,
		ForkLSN:    branch.BaseLSN,
	}, nil
}

// Adopt turns an existing, not-yet-live branch into a sandbox: stamps the
// TTL and checks it out. This is how sandboxes are created from a
// historical point (a pin) — the branch is forked at the pinned LSN by the
// restore service first, then adopted here.
func (s *Service) Adopt(ctx context.Context, branchID string, ttl time.Duration) (*Info, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("branch not found: %w", err)
	}
	forkedFrom := ""
	if branch.ParentID != "" {
		if parent, err := s.branches.GetBranchByID(branch.ParentID); err == nil {
			forkedFrom = parent.Name
		}
	}

	expires := time.Now().Add(ttl)
	if err := s.branches.SetExpiry(branch.ID, &expires); err != nil {
		return nil, fmt.Errorf("failed to stamp sandbox TTL: %w", err)
	}
	info, err := s.checkout.Checkout(ctx, branch.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to check the sandbox out: %w", err)
	}
	return &Info{
		BranchID:   branch.ID,
		BranchName: branch.Name,
		PhysicalDB: info.PhysicalDB,
		ExpiresAt:  expires,
		ForkedFrom: forkedFrom,
		ForkLSN:    branch.BaseLSN,
	}, nil
}

// Extend pushes a sandbox's expiry further out from now.
func (s *Service) Extend(ctx context.Context, branchID string, ttl time.Duration) (time.Time, error) {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return time.Time{}, fmt.Errorf("sandbox not found: %w", err)
	}
	if branch.ExpiresAt == nil {
		return time.Time{}, fmt.Errorf("branch %s is not a sandbox (no TTL)", branch.Name)
	}
	expires := time.Now().Add(ttl)
	if err := s.branches.SetExpiry(branch.ID, &expires); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

// Keep removes the TTL, converting the sandbox into an ordinary branch.
func (s *Service) Keep(ctx context.Context, branchID string) error {
	return s.branches.SetExpiry(branchID, nil)
}

// Discard releases and deletes a sandbox immediately. Deletion reclaims
// its WAL entries and snapshots through the branch delete hook.
func (s *Service) Discard(ctx context.Context, branchID string) error {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return fmt.Errorf("sandbox not found: %w", err)
	}
	if branch.IsLive() {
		if err := s.checkout.Release(ctx, branch.ID); err != nil {
			return fmt.Errorf("failed to release the sandbox: %w", err)
		}
	}
	return s.branches.DeleteBranch(branch.ProjectID, branch.Name)
}

// SweepReport summarizes one reaping pass.
type SweepReport struct {
	Reaped  []string // branch names removed
	Skipped []string // expired but blocked (e.g. live children)
}

// Sweep discards every expired sandbox in a project. Expired sandboxes
// with live children are skipped loudly — children pin their parents.
func (s *Service) Sweep(ctx context.Context, projectID string) (*SweepReport, error) {
	branches, err := s.branches.ListBranches(projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	now := time.Now()
	report := &SweepReport{}
	for _, branch := range branches {
		if !branch.IsExpired(now) {
			continue
		}
		if err := s.Discard(ctx, branch.ID); err != nil {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%v)", branch.Name, err))
			continue
		}
		report.Reaped = append(report.Reaped, branch.Name)
	}
	return report, nil
}

// ListSandboxes returns a project's live sandbox branches (TTL-stamped).
func (s *Service) ListSandboxes(ctx context.Context, projectID string) ([]*wal.Branch, error) {
	branches, err := s.branches.ListBranches(projectID)
	if err != nil {
		return nil, err
	}
	sandboxes := make([]*wal.Branch, 0)
	for _, b := range branches {
		if b.ExpiresAt != nil {
			sandboxes = append(sandboxes, b)
		}
	}
	return sandboxes, nil
}
