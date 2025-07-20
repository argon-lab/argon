package restore

import (
	"fmt"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Service provides branch restore and creation from historical points
type Service struct {
	wal          *wal.Service
	branches     *branchwal.BranchService
	materializer *materializer.Service
	timeTravel   *timetravel.Service
}

// NewService creates a new restore service
func NewService(
	walService *wal.Service,
	branchService *branchwal.BranchService,
	materializerService *materializer.Service,
	timeTravelService *timetravel.Service,
) *Service {
	return &Service{
		wal:          walService,
		branches:     branchService,
		materializer: materializerService,
		timeTravel:   timeTravelService,
	}
}

// ResetBranchToLSN resets a branch to a historical LSN
func (s *Service) ResetBranchToLSN(branchID string, targetLSN int64) (*wal.Branch, error) {
	// Get the branch
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch: %w", err)
	}

	// Validate target LSN
	if targetLSN < branch.BaseLSN {
		return nil, fmt.Errorf("target LSN %d is before branch base LSN %d", targetLSN, branch.BaseLSN)
	}

	if targetLSN > branch.HeadLSN {
		return nil, fmt.Errorf("target LSN %d is beyond branch HEAD %d", targetLSN, branch.HeadLSN)
	}

	// Safety check: warn if resetting would lose data
	entriesAfterTarget, err := s.wal.GetBranchEntries(branchID, "", targetLSN+1, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to check entries after target: %w", err)
	}

	if len(entriesAfterTarget) > 0 {
		// In production, you might want to create a backup branch or require confirmation
		// For now, we'll proceed but log a warning
		fmt.Printf("WARNING: Resetting branch %s to LSN %d will discard %d operations\n", 
			branch.Name, targetLSN, len(entriesAfterTarget))
	}

	// Update branch HEAD
	branch.HeadLSN = targetLSN

	// Save the updated branch
	if err := s.branches.UpdateBranchHead(branchID, targetLSN); err != nil {
		return nil, fmt.Errorf("failed to update branch HEAD: %w", err)
	}

	return branch, nil
}

// ResetBranchToTime resets a branch to a specific timestamp
func (s *Service) ResetBranchToTime(branchID string, timestamp time.Time) (*wal.Branch, error) {
	// Get the branch
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch: %w", err)
	}

	// Find the LSN at the target time
	targetLSN, err := s.timeTravel.FindLSNAtTime(branch, timestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to find LSN at time: %w", err)
	}

	// Reset to that LSN
	return s.ResetBranchToLSN(branchID, targetLSN)
}

// CreateBranchAtLSN creates a new branch from a historical point
func (s *Service) CreateBranchAtLSN(projectID, sourceBranchID, newBranchName string, targetLSN int64) (*wal.Branch, error) {
	// Get the source branch
	sourceBranch, err := s.branches.GetBranchByID(sourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("failed to get source branch: %w", err)
	}

	// Validate target LSN
	if targetLSN < sourceBranch.BaseLSN || targetLSN > sourceBranch.HeadLSN {
		return nil, fmt.Errorf("target LSN %d is outside source branch range [%d, %d]", 
			targetLSN, sourceBranch.BaseLSN, sourceBranch.HeadLSN)
	}

	// Create the new branch
	newBranch := &wal.Branch{
		ID:        primitive.NewObjectID().Hex(),
		ProjectID: projectID,
		Name:      newBranchName,
		BaseLSN:   targetLSN,
		HeadLSN:   targetLSN,
		CreatedAt: time.Now(),
	}

	// Save the branch
	if err := s.branches.CreateBranchWithData(newBranch); err != nil {
		return nil, fmt.Errorf("failed to create branch: %w", err)
	}

	return newBranch, nil
}

// CreateBranchAtTime creates a new branch from a specific timestamp
func (s *Service) CreateBranchAtTime(projectID, sourceBranchID, newBranchName string, timestamp time.Time) (*wal.Branch, error) {
	// Get the source branch
	sourceBranch, err := s.branches.GetBranchByID(sourceBranchID)
	if err != nil {
		return nil, fmt.Errorf("failed to get source branch: %w", err)
	}

	// Find the LSN at the target time
	targetLSN, err := s.timeTravel.FindLSNAtTime(sourceBranch, timestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to find LSN at time: %w", err)
	}

	// Create branch at that LSN
	return s.CreateBranchAtLSN(projectID, sourceBranchID, newBranchName, targetLSN)
}

// GetRestorePreview shows what a restore operation would do
func (s *Service) GetRestorePreview(branchID string, targetLSN int64) (*RestorePreview, error) {
	// Get the branch
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch: %w", err)
	}

	// Validate target LSN
	if targetLSN < branch.BaseLSN || targetLSN > branch.HeadLSN {
		return nil, fmt.Errorf("invalid target LSN %d for branch range [%d, %d]", 
			targetLSN, branch.BaseLSN, branch.HeadLSN)
	}

	// Get entries that would be discarded
	discardedEntries, err := s.wal.GetBranchEntries(branchID, "", targetLSN+1, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get discarded entries: %w", err)
	}

	// Get collections that would be affected
	affectedCollections := make(map[string]int)
	for _, entry := range discardedEntries {
		if entry.Collection != "" {
			affectedCollections[entry.Collection]++
		}
	}

	// Get current and target state summaries
	currentCollections, err := s.timeTravel.FindModifiedCollections(branch, branch.BaseLSN, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get current collections: %w", err)
	}

	targetCollections, err := s.timeTravel.FindModifiedCollections(branch, branch.BaseLSN, targetLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get target collections: %w", err)
	}

	return &RestorePreview{
		BranchID:            branchID,
		BranchName:          branch.Name,
		CurrentLSN:          branch.HeadLSN,
		TargetLSN:           targetLSN,
		OperationsToDiscard: len(discardedEntries),
		AffectedCollections: affectedCollections,
		CurrentCollections:  currentCollections,
		TargetCollections:   targetCollections,
	}, nil
}

// RestorePreview contains information about what a restore would do
type RestorePreview struct {
	BranchID            string
	BranchName          string
	CurrentLSN          int64
	TargetLSN           int64
	OperationsToDiscard int
	AffectedCollections map[string]int // collection -> operation count
	CurrentCollections  []string
	TargetCollections   []string
}

// ValidateRestore checks if a restore operation is safe
func (s *Service) ValidateRestore(branchID string, targetLSN int64) error {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return fmt.Errorf("failed to get branch: %w", err)
	}

	// Check LSN range
	if targetLSN < branch.BaseLSN {
		return fmt.Errorf("cannot restore to LSN %d before branch creation (base LSN: %d)", 
			targetLSN, branch.BaseLSN)
	}

	if targetLSN > branch.HeadLSN {
		return fmt.Errorf("cannot restore to future LSN %d (current HEAD: %d)", 
			targetLSN, branch.HeadLSN)
	}

	// Check if branch has dependent branches (future enhancement)
	// For MVP, we assume branches are independent

	return nil
}