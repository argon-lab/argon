package timetravel

import (
	"fmt"
	"time"

	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// Service provides time travel capabilities for WAL-based branches. All
// state reconstruction delegates to the materializer, which owns ancestry
// traversal and deterministic replay; this service adds LSN validation and
// timestamp-to-LSN resolution on top.
type Service struct {
	wal          *wal.Service
	materializer *materializer.Service
}

// NewService creates a new time travel service
func NewService(walService *wal.Service, materializerService *materializer.Service) *Service {
	return &Service{
		wal:          walService,
		materializer: materializerService,
	}
}

// MaterializeAtLSN reconstructs the state of a collection at a specific LSN
func (s *Service) MaterializeAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error) {
	if err := s.validateTargetLSN(branch, targetLSN); err != nil {
		return nil, err
	}
	return s.materializer.MaterializeCollectionAtLSN(branch, collection, targetLSN)
}

// MaterializeAtTime reconstructs the state of a collection at a specific timestamp
func (s *Service) MaterializeAtTime(branch *wal.Branch, collection string, timestamp time.Time) (map[string]bson.M, error) {
	targetLSN, err := s.FindLSNAtTime(branch, timestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to find LSN at time: %w", err)
	}
	return s.MaterializeAtLSN(branch, collection, targetLSN)
}

// FindLSNAtTime finds the latest LSN on the branch at or before the given
// timestamp.
func (s *Service) FindLSNAtTime(branch *wal.Branch, timestamp time.Time) (int64, error) {
	if timestamp.After(time.Now()) {
		return 0, fmt.Errorf("cannot find LSN for future timestamp %v", timestamp)
	}

	entries, err := s.wal.GetEntriesByTimestamp(branch.ProjectID, timestamp)
	if err != nil {
		return 0, fmt.Errorf("failed to get entries by timestamp: %w", err)
	}

	// Filter for this branch and find the latest LSN
	var latestLSN int64
	for _, entry := range entries {
		if entry.BranchID == branch.ID && entry.LSN > latestLSN && entry.LSN <= branch.HeadLSN {
			latestLSN = entry.LSN
		}
	}

	if latestLSN == 0 {
		return 0, fmt.Errorf("no entries found before timestamp %v", timestamp)
	}

	return latestLSN, nil
}

// GetBranchStateAtLSN returns the complete state of all collections at a specific LSN
func (s *Service) GetBranchStateAtLSN(branch *wal.Branch, targetLSN int64) (map[string]map[string]bson.M, error) {
	if err := s.validateTargetLSN(branch, targetLSN); err != nil {
		return nil, err
	}
	return s.materializer.MaterializeBranchAtLSN(branch, targetLSN)
}

// GetDocumentHistoryAtLSN returns the history of a document up to a specific LSN
func (s *Service) GetDocumentHistoryAtLSN(branch *wal.Branch, collection, documentID string, targetLSN int64) ([]*wal.Entry, error) {
	if err := s.validateTargetLSN(branch, targetLSN); err != nil {
		return nil, err
	}
	return s.wal.GetDocumentHistory(branch.ID, collection, documentID, 0, targetLSN)
}

// FindModifiedCollections returns collections that were modified between two LSNs
func (s *Service) FindModifiedCollections(branch *wal.Branch, fromLSN, toLSN int64) ([]string, error) {
	entries, err := s.wal.GetBranchEntries(branch.ID, "", fromLSN, toLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get WAL entries: %w", err)
	}

	collections := make(map[string]bool)
	for _, entry := range entries {
		if entry.Collection != "" {
			collections[entry.Collection] = true
		}
	}

	result := make([]string, 0, len(collections))
	for col := range collections {
		result = append(result, col)
	}

	return result, nil
}

// GetTimeTravelInfo returns metadata about available time travel range
func (s *Service) GetTimeTravelInfo(branch *wal.Branch) (*TimeTravelInfo, error) {
	allEntries, err := s.wal.GetBranchEntries(branch.ID, "", 0, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get WAL entries: %w", err)
	}

	// Only data entries are meaningful travel points; control records
	// (branch creation etc.) carry no document state.
	entries := allEntries[:0]
	for _, entry := range allEntries {
		if entry.IsData() {
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		return &TimeTravelInfo{
			BranchID:    branch.ID,
			BranchName:  branch.Name,
			EarliestLSN: branch.BaseLSN,
			LatestLSN:   branch.HeadLSN,
			EntryCount:  0,
		}, nil
	}

	return &TimeTravelInfo{
		BranchID:     branch.ID,
		BranchName:   branch.Name,
		EarliestLSN:  entries[0].LSN,
		LatestLSN:    entries[len(entries)-1].LSN,
		EarliestTime: entries[0].Timestamp,
		LatestTime:   entries[len(entries)-1].Timestamp,
		EntryCount:   len(entries),
	}, nil
}

func (s *Service) validateTargetLSN(branch *wal.Branch, targetLSN int64) error {
	if targetLSN < 0 {
		return fmt.Errorf("invalid LSN: %d", targetLSN)
	}
	if targetLSN > branch.HeadLSN {
		return fmt.Errorf("target LSN %d is beyond branch HEAD %d", targetLSN, branch.HeadLSN)
	}
	return nil
}

// TimeTravelInfo contains metadata about time travel capabilities
type TimeTravelInfo struct {
	BranchID     string
	BranchName   string
	EarliestLSN  int64
	LatestLSN    int64
	EarliestTime time.Time
	LatestTime   time.Time
	EntryCount   int
}
