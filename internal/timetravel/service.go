package timetravel

import (
	"fmt"
	"time"

	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// Service provides time travel capabilities for WAL-based branches
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
	// Validate LSN range
	if targetLSN < 0 {
		return nil, fmt.Errorf("invalid LSN: %d", targetLSN)
	}
	
	if targetLSN > branch.HeadLSN {
		return nil, fmt.Errorf("target LSN %d is beyond branch HEAD %d", targetLSN, branch.HeadLSN)
	}
	
	// For branches created from a historical point, include base entries
	entries := []*wal.Entry{}
	
	if branch.BaseLSN > 0 {
		// Get entries up to the base LSN from the global WAL
		maxLSN := branch.BaseLSN
		if targetLSN < maxLSN {
			maxLSN = targetLSN
		}
		baseEntries, err := s.wal.GetProjectEntries(branch.ProjectID, collection, 0, maxLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get base entries: %w", err)
		}
		entries = append(entries, baseEntries...)
	}
	
	// Get entries specific to this branch up to target LSN
	if targetLSN > branch.BaseLSN {
		branchEntries, err := s.wal.GetBranchEntries(branch.ID, collection, branch.BaseLSN+1, targetLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get branch entries: %w", err)
		}
		entries = append(entries, branchEntries...)
	}
	
	// Build state by replaying entries
	state := make(map[string]bson.M)
	for _, entry := range entries {
		if err := s.materializer.ApplyEntry(state, entry); err != nil {
			return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
	}
	
	return state, nil
}

// MaterializeAtTime reconstructs the state of a collection at a specific timestamp
func (s *Service) MaterializeAtTime(branch *wal.Branch, collection string, timestamp time.Time) (map[string]bson.M, error) {
	// Find the LSN at the given timestamp
	targetLSN, err := s.FindLSNAtTime(branch, timestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to find LSN at time: %w", err)
	}
	
	// Materialize at that LSN
	return s.MaterializeAtLSN(branch, collection, targetLSN)
}

// FindLSNAtTime finds the latest LSN before or at the given timestamp
func (s *Service) FindLSNAtTime(branch *wal.Branch, timestamp time.Time) (int64, error) {
	// Check if timestamp is in the future
	if timestamp.After(time.Now()) {
		return 0, fmt.Errorf("cannot find LSN for future timestamp %v", timestamp)
	}
	
	// Get entries for the branch up to the timestamp
	entries, err := s.wal.GetEntriesByTimestamp(branch.ProjectID, timestamp)
	if err != nil {
		return 0, fmt.Errorf("failed to get entries by timestamp: %w", err)
	}
	
	// Filter for this branch and find the latest LSN
	var latestLSN int64 = 0
	for _, entry := range entries {
		if entry.BranchID == branch.ID && entry.LSN > latestLSN {
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
	// Validate LSN
	if targetLSN < 0 || targetLSN > branch.HeadLSN {
		return nil, fmt.Errorf("invalid target LSN %d (branch HEAD: %d)", targetLSN, branch.HeadLSN)
	}
	
	// For branches created from a historical point, include base entries
	entries := []*wal.Entry{}
	
	if branch.BaseLSN > 0 {
		// Get entries up to the base LSN from the global WAL
		maxLSN := branch.BaseLSN
		if targetLSN < maxLSN {
			maxLSN = targetLSN
		}
		baseEntries, err := s.wal.GetProjectEntries(branch.ProjectID, "", 0, maxLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get base entries: %w", err)
		}
		entries = append(entries, baseEntries...)
	}
	
	// Get entries specific to this branch up to target LSN
	if targetLSN > branch.BaseLSN {
		branchEntries, err := s.wal.GetBranchEntries(branch.ID, "", branch.BaseLSN+1, targetLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get branch entries: %w", err)
		}
		entries = append(entries, branchEntries...)
	}
	
	// Build state by collection
	state := make(map[string]map[string]bson.M)
	
	for _, entry := range entries {
		if entry.Collection == "" {
			continue // Skip non-collection operations
		}
		
		// Initialize collection state if needed
		if _, exists := state[entry.Collection]; !exists {
			state[entry.Collection] = make(map[string]bson.M)
		}
		
		if err := s.materializer.ApplyEntry(state[entry.Collection], entry); err != nil {
			return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
	}
	
	return state, nil
}

// GetDocumentHistoryAtLSN returns the history of a document up to a specific LSN
func (s *Service) GetDocumentHistoryAtLSN(branch *wal.Branch, collection, documentID string, targetLSN int64) ([]*wal.Entry, error) {
	// Validate LSN
	if targetLSN < 0 || targetLSN > branch.HeadLSN {
		return nil, fmt.Errorf("invalid target LSN %d (branch HEAD: %d)", targetLSN, branch.HeadLSN)
	}
	
	// Get document history up to target LSN
	return s.wal.GetDocumentHistory(branch.ID, collection, documentID, 0, targetLSN)
}

// FindModifiedCollections returns collections that were modified between two LSNs
func (s *Service) FindModifiedCollections(branch *wal.Branch, fromLSN, toLSN int64) ([]string, error) {
	// Get entries in the LSN range
	entries, err := s.wal.GetBranchEntries(branch.ID, "", fromLSN, toLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get WAL entries: %w", err)
	}
	
	// Track unique collections
	collections := make(map[string]bool)
	for _, entry := range entries {
		if entry.Collection != "" {
			collections[entry.Collection] = true
		}
	}
	
	// Convert to slice
	result := make([]string, 0, len(collections))
	for col := range collections {
		result = append(result, col)
	}
	
	return result, nil
}

// GetTimeTravelInfo returns metadata about available time travel range
func (s *Service) GetTimeTravelInfo(branch *wal.Branch) (*TimeTravelInfo, error) {
	// Get first and last entries for the branch
	entries, err := s.wal.GetBranchEntries(branch.ID, "", 0, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get WAL entries: %w", err)
	}
	
	if len(entries) == 0 {
		return &TimeTravelInfo{
			BranchID:      branch.ID,
			BranchName:    branch.Name,
			EarliestLSN:   branch.BaseLSN,
			LatestLSN:     branch.HeadLSN,
			EntryCount:    0,
		}, nil
	}
	
	return &TimeTravelInfo{
		BranchID:       branch.ID,
		BranchName:     branch.Name,
		EarliestLSN:    entries[0].LSN,
		LatestLSN:      entries[len(entries)-1].LSN,
		EarliestTime:   entries[0].Timestamp,
		LatestTime:     entries[len(entries)-1].Timestamp,
		EntryCount:     len(entries),
	}, nil
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

