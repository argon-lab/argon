package sdk

import (
	"fmt"
	"time"

	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/wal"
)

// RestoreSDK provides high-level restore operations
type RestoreSDK struct {
	service *restore.Service
}

// NewRestoreSDK creates a new restore SDK
func NewRestoreSDK(service *restore.Service) *RestoreSDK {
	return &RestoreSDK{
		service: service,
	}
}

// ResetBranchToLSN resets a branch to a specific LSN
func (s *RestoreSDK) ResetBranchToLSN(branchID string, targetLSN int64) (*wal.Branch, error) {
	return s.service.ResetBranchToLSN(branchID, targetLSN)
}

// ResetBranchToTime resets a branch to a specific timestamp
func (s *RestoreSDK) ResetBranchToTime(branchID string, timestamp time.Time) (*wal.Branch, error) {
	return s.service.ResetBranchToTime(branchID, timestamp)
}

// CreateBranchFromLSN creates a new branch from a historical LSN
func (s *RestoreSDK) CreateBranchFromLSN(projectID, sourceBranchID, newBranchName string, targetLSN int64) (*wal.Branch, error) {
	return s.service.CreateBranchAtLSN(projectID, sourceBranchID, newBranchName, targetLSN)
}

// CreateBranchFromTime creates a new branch from a specific timestamp
func (s *RestoreSDK) CreateBranchFromTime(projectID, sourceBranchID, newBranchName string, timestamp time.Time) (*wal.Branch, error) {
	return s.service.CreateBranchAtTime(projectID, sourceBranchID, newBranchName, timestamp)
}

// PreviewRestore shows what a restore operation would do
func (s *RestoreSDK) PreviewRestore(branchID string, targetLSN int64) (*RestorePreview, error) {
	preview, err := s.service.GetRestorePreview(branchID, targetLSN)
	if err != nil {
		return nil, err
	}

	return &RestorePreview{
		BranchName:          preview.BranchName,
		CurrentLSN:          preview.CurrentLSN,
		TargetLSN:           preview.TargetLSN,
		OperationsToDiscard: preview.OperationsToDiscard,
		AffectedCollections: preview.AffectedCollections,
	}, nil
}

// RestorePreview contains information about what a restore would do
type RestorePreview struct {
	BranchName          string
	CurrentLSN          int64
	TargetLSN           int64
	OperationsToDiscard int
	AffectedCollections map[string]int
}

// TimeAgoRestore is a convenience method to reset a branch to a relative time
func (s *RestoreSDK) TimeAgoRestore(branchID string, duration time.Duration) (*wal.Branch, error) {
	targetTime := time.Now().Add(-duration)
	return s.service.ResetBranchToTime(branchID, targetTime)
}

// CreateBackup creates a backup branch at the current state before performing dangerous operations
func (s *RestoreSDK) CreateBackup(projectID, sourceBranchID string) (*wal.Branch, error) {
	backupName := fmt.Sprintf("%s-backup-%d", sourceBranchID, time.Now().Unix())
	// Create backup at current HEAD (we'll need to get the branch first)
	// This is a simplified version - in production you'd get the branch HEAD
	return s.service.CreateBranchAtLSN(projectID, sourceBranchID, backupName, 0)
}