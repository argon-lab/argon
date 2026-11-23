// Package sandbox manages checked-out branches with an optional scoped
// MongoDB credential. A sweep reclaims expired branches.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/mongo"
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
	BranchID      string
	BranchName    string
	PhysicalDB    string
	ExpiresAt     time.Time
	ForkedFrom    string
	ForkLSN       int64
	CredentialURI string `json:"-" bson:"-"`
	// Indexes and IndexNotes report the secondary indexes the checkout built.
	Indexes    int
	IndexNotes []string
}

// Create forks a sandbox branch from the given parent, checks it out and
// stamps the TTL. The returned physical database is what the agent
// connects to; run the ingester (argon watch) to capture its writes.
func (s *Service) Create(ctx context.Context, projectID, parentBranchID, name string, ttl time.Duration) (*Info, error) {
	return s.create(ctx, projectID, parentBranchID, name, ttl, nil)
}

// CreateWithCredential issues one database-local MongoDB user after checkout.
func (s *Service) CreateWithCredential(ctx context.Context, projectID, parentBranchID, name string, ttl time.Duration, baseURI string) (*Info, error) {
	if err := s.checkout.RequireSandboxAuthentication(ctx); err != nil {
		return nil, err
	}
	return s.create(ctx, projectID, parentBranchID, name, ttl, &baseURI)
}

func (s *Service) create(ctx context.Context, projectID, parentBranchID, name string, ttl time.Duration, baseURI *string) (*Info, error) {
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

	var branch *wal.Branch
	var expires time.Time
	if baseURI != nil {
		expires = time.Now().Add(ttl)
		branch, err = s.branches.CreateCredentialSandbox(projectID, name, parent.ID, expires)
	} else {
		branch, err = s.branches.CreateBranch(projectID, name, parent.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fork sandbox: %w", err)
	}

	if baseURI == nil {
		expires = time.Now().Add(ttl)
		if err := s.branches.SetExpiry(branch.ID, &expires); err != nil {
			return nil, s.removeFailed(branch, false, fmt.Errorf("failed to stamp sandbox TTL: %w", err))
		}
	}

	info, err := s.checkout.Checkout(ctx, branch.ID)
	if err != nil {
		return nil, s.removeFailed(branch, false, fmt.Errorf("failed to check the sandbox out: %w", err))
	}

	var uri string
	if baseURI != nil {
		var mayExist bool
		uri, mayExist, err = s.checkout.IssueSandboxCredential(ctx, branch, *baseURI)
		if err != nil {
			return nil, s.removeFailed(branch, mayExist, err)
		}
	}
	return &Info{
		BranchID:      branch.ID,
		BranchName:    branch.Name,
		PhysicalDB:    info.PhysicalDB,
		Indexes:       info.Indexes,
		IndexNotes:    info.IndexNotes,
		ExpiresAt:     expires,
		ForkedFrom:    parent.Name,
		ForkLSN:       branch.BaseLSN,
		CredentialURI: uri,
	}, nil
}

// Adopt turns an existing, not-yet-live branch into a sandbox: stamps the
// TTL and checks it out. This is how sandboxes are created from a
// historical point (a pin) — the branch is forked at the pinned LSN by the
// restore service first, then adopted here. Adopt owns that branch from
// then on: if it fails, the branch is removed, as a failed Create removes
// its fork.
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
		return nil, s.removeFailed(branch, true, fmt.Errorf("failed to stamp sandbox TTL: %w", err))
	}
	info, err := s.checkout.Checkout(ctx, branch.ID)
	if err != nil {
		return nil, s.removeFailed(branch, true, fmt.Errorf("failed to check the sandbox out: %w", err))
	}
	return &Info{
		BranchID:   branch.ID,
		BranchName: branch.Name,
		PhysicalDB: info.PhysicalDB,
		Indexes:    info.Indexes,
		IndexNotes: info.IndexNotes,
		ExpiresAt:  expires,
		ForkedFrom: forkedFrom,
		ForkLSN:    branch.BaseLSN,
	}, nil
}

// removeFailed cleans up with a fresh context because the create request
// may have been interrupted after MongoDB created the user.
func (s *Service) removeFailed(branch *wal.Branch, userMayExist bool, cause error) error {
	if branch.SandboxUsername != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := s.checkout.DiscardFailedSandbox(ctx, branch.ID, userMayExist)
		if err != nil {
			// If the branch is gone, someone discarded it while it was
			// being set up, whatever step of the cleanup noticed: nothing
			// of this create is left, and its name may be another
			// sandbox's by now.
			if _, getErr := s.branches.GetBranchByID(branch.ID); errors.Is(getErr, mongo.ErrNoDocuments) {
				return cause
			}
			return fmt.Errorf("%w; cleanup of sandbox %q failed (%v): retry argon sandbox discard -b %s", cause, branch.Name, err, branch.Name)
		}
	}
	// By ID: the name may belong to another sandbox by now, created after
	// someone discarded this one while it was still being set up.
	err := s.branches.DeleteBranchByID(branch.ID)
	if errors.Is(err, branchwal.ErrBranchNotFound) {
		// Already removed; nothing of this create is left.
		return cause
	}
	if err != nil {
		return fmt.Errorf("%w; the sandbox branch %q was left behind and could not be removed (%v): remove it with argon sandbox discard -b %s", cause, branch.Name, err, branch.Name)
	}
	return cause
}

// Extend pushes a sandbox's expiry further out from now.
func (s *Service) Extend(ctx context.Context, branchID string, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, fmt.Errorf("sandbox TTL extension must be positive")
	}
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
	return s.discard(ctx, branchID, nil)
}

func (s *Service) discard(ctx context.Context, branchID string, expiredBefore *time.Time) error {
	// Another discard, or the sweeper, may remove the sandbox at any of
	// these steps; each then answers "branch not found", not the driver's
	// "no documents".
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return fmt.Errorf("sandbox not found: %w", branchwal.NotFound(err))
	}
	if err := s.branches.RequireDeletable(branchID); err != nil {
		return branchwal.NotFound(err)
	}
	if branch.IsLive() || branch.SandboxUsername != "" {
		if expiredBefore != nil && branch.SandboxUsername != "" {
			err = s.checkout.DiscardExpired(ctx, branch.ID, *expiredBefore)
		} else {
			err = s.checkout.Discard(ctx, branch.ID)
		}
		if err != nil {
			return fmt.Errorf("failed to release the sandbox: %w", branchwal.NotFound(err))
		}
	}
	// By ID, never by name: another sandbox may have taken the name.
	return s.branches.DeleteBranchByID(branch.ID)
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
		if err := s.discard(ctx, branch.ID, &now); err != nil {
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
