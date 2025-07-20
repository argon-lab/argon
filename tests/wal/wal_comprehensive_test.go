package wal_test

import (
	"fmt"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWALService_EdgeCases(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	t.Run("LSN is strictly increasing", func(t *testing.T) {
		// Append multiple entries rapidly
		lsns := make([]int64, 10)
		for i := 0; i < 10; i++ {
			entry := &wal.Entry{
				ProjectID:  "test",
				BranchID:   "main",
				Operation:  wal.OpInsert,
				Collection: "test",
				DocumentID: fmt.Sprintf("doc-%d", i),
			}
			lsns[i], err = walService.Append(entry)
			require.NoError(t, err)
		}

		// Verify strict ordering
		for i := 1; i < len(lsns); i++ {
			assert.Equal(t, lsns[i-1]+1, lsns[i], "LSN should be strictly increasing")
		}
	})

	t.Run("Concurrent appends maintain LSN uniqueness", func(t *testing.T) {
		done := make(chan int64, 100)
		
		// Launch concurrent appends
		for i := 0; i < 100; i++ {
			go func(id int) {
				entry := &wal.Entry{
					ProjectID:  "concurrent",
					BranchID:   "main",
					Operation:  wal.OpInsert,
					DocumentID: fmt.Sprintf("doc-%d", id),
				}
				lsn, err := walService.Append(entry)
				assert.NoError(t, err)
				done <- lsn
			}(i)
		}

		// Collect all LSNs
		lsns := make(map[int64]bool)
		for i := 0; i < 100; i++ {
			lsn := <-done
			assert.False(t, lsns[lsn], "LSN %d was duplicated", lsn)
			lsns[lsn] = true
		}
		
		assert.Len(t, lsns, 100, "Should have 100 unique LSNs")
	})

	t.Run("GetBranchEntries respects LSN range", func(t *testing.T) {
		// Create entries
		for i := 0; i < 10; i++ {
			entry := &wal.Entry{
				ProjectID:  "range-test",
				BranchID:   "test-branch",
				Operation:  wal.OpInsert,
				Collection: "items",
				DocumentID: fmt.Sprintf("item-%d", i),
			}
			_, err := walService.Append(entry)
			require.NoError(t, err)
		}

		// Get current LSN
		currentLSN := walService.GetCurrentLSN()

		// Query specific range
		entries, err := walService.GetBranchEntries("test-branch", "items", currentLSN-5, currentLSN-2)
		assert.NoError(t, err)
		assert.Len(t, entries, 4, "Should get entries in range")
	})
}

func TestBranchService_ComplexScenarios(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	t.Run("Branch hierarchy tracking", func(t *testing.T) {
		// Create project and main branch
		main, err := branchService.CreateBranch("proj-1", "main", "")
		require.NoError(t, err)
		
		// Create feature branch from main
		feature1, err := branchService.CreateBranch("proj-1", "feature-1", main.ID)
		require.NoError(t, err)
		assert.Equal(t, main.HeadLSN, feature1.BaseLSN, "Feature should fork at main's HEAD")
		
		// Create sub-feature from feature-1
		subFeature, err := branchService.CreateBranch("proj-1", "sub-feature", feature1.ID)
		require.NoError(t, err)
		assert.Equal(t, feature1.HeadLSN, subFeature.BaseLSN, "Sub-feature should fork at feature-1's HEAD")
		
		// Verify parent relationships
		assert.Equal(t, "", main.ParentID)
		assert.Equal(t, main.ID, feature1.ParentID)
		assert.Equal(t, feature1.ID, subFeature.ParentID)
	})

	t.Run("Cannot create duplicate branches", func(t *testing.T) {
		_, err := branchService.CreateBranch("proj-2", "main", "")
		require.NoError(t, err)
		
		// Try to create duplicate
		_, err = branchService.CreateBranch("proj-2", "main", "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("Branch deletion with complex hierarchy", func(t *testing.T) {
		// Create hierarchy: main -> dev -> feature -> bugfix
		main, _ := branchService.CreateBranch("proj-3", "main", "")
		dev, _ := branchService.CreateBranch("proj-3", "dev", main.ID)
		feature, _ := branchService.CreateBranch("proj-3", "feature", dev.ID)
		_, _ = branchService.CreateBranch("proj-3", "bugfix", feature.ID)
		
		// Cannot delete branch with children
		err := branchService.DeleteBranch("proj-3", "feature")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "active children")
		
		// Can delete leaf branch
		err = branchService.DeleteBranch("proj-3", "bugfix")
		assert.NoError(t, err)
		
		// Now can delete feature
		err = branchService.DeleteBranch("proj-3", "feature")
		assert.NoError(t, err)
	})

	t.Run("UpdateBranchHead maintains consistency", func(t *testing.T) {
		branch, err := branchService.CreateBranch("proj-4", "test", "")
		require.NoError(t, err)
		
		initialHead := branch.HeadLSN
		
		// Simulate operations advancing the branch
		for i := 0; i < 5; i++ {
			entry := &wal.Entry{
				ProjectID:  "proj-4",
				BranchID:   branch.ID,
				Operation:  wal.OpInsert,
				Collection: "data",
			}
			lsn, _ := walService.Append(entry)
			
			// Update branch head
			err = branchService.UpdateBranchHead(branch.ID, lsn)
			assert.NoError(t, err)
			
			// Verify update
			updated, _ := branchService.GetBranchByID(branch.ID)
			assert.Equal(t, lsn, updated.HeadLSN)
		}
		
		// Head should have advanced
		finalBranch, _ := branchService.GetBranchByID(branch.ID)
		assert.Greater(t, finalBranch.HeadLSN, initialHead)
	})
}

func TestProjectService_EdgeCases(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)

	t.Run("Project creation creates main branch", func(t *testing.T) {
		project, err := projectService.CreateProject("test-project")
		require.NoError(t, err)
		
		// Verify main branch exists
		branches, err := branchService.ListBranches(project.ID)
		assert.NoError(t, err)
		assert.Len(t, branches, 1)
		assert.Equal(t, "main", branches[0].Name)
		assert.Equal(t, project.MainBranchID, branches[0].ID)
	})

	t.Run("Cannot create duplicate projects", func(t *testing.T) {
		_, err := projectService.CreateProject("unique-name")
		require.NoError(t, err)
		
		_, err = projectService.CreateProject("unique-name")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("Delete project cascades to branches", func(t *testing.T) {
		project, _ := projectService.CreateProject("cascade-test")
		
		// Create additional branches
		branches, _ := branchService.ListBranches(project.ID)
		mainBranch := branches[0]
		branchService.CreateBranch(project.ID, "feature-1", mainBranch.ID)
		branchService.CreateBranch(project.ID, "feature-2", mainBranch.ID)
		
		// Delete project
		err := projectService.DeleteProject(project.ID)
		assert.NoError(t, err)
		
		// Verify branches are gone
		branches, _ = branchService.ListBranches(project.ID)
		assert.Len(t, branches, 0)
		
		// Verify project is gone
		_, err = projectService.GetProject(project.ID)
		assert.Error(t, err)
	})
}

func TestWALService_TimestampOrdering(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)

	t.Run("Timestamps are monotonically increasing", func(t *testing.T) {
		var lastTimestamp time.Time
		
		for i := 0; i < 10; i++ {
			entry := &wal.Entry{
				ProjectID: "time-test",
				BranchID:  "main",
				Operation: wal.OpInsert,
			}
			lsn, _ := walService.Append(entry)
			
			// Get the entry back
			saved, err := walService.GetEntry(lsn)
			assert.NoError(t, err)
			
			if i > 0 {
				assert.True(t, saved.Timestamp.After(lastTimestamp) || saved.Timestamp.Equal(lastTimestamp),
					"Timestamp should not go backwards")
			}
			lastTimestamp = saved.Timestamp
		}
	})

	t.Run("GetEntriesByTimestamp works correctly", func(t *testing.T) {
		projectID := "timestamp-project"
		
		// Create entries with slight delays
		var midTimestamp time.Time
		for i := 0; i < 5; i++ {
			entry := &wal.Entry{
				ProjectID: projectID,
				BranchID:  "main",
				Operation: wal.OpInsert,
			}
			walService.Append(entry)
			
			if i == 2 {
				midTimestamp = time.Now()
			}
			time.Sleep(10 * time.Millisecond)
		}
		
		// Query up to mid timestamp
		entries, err := walService.GetEntriesByTimestamp(projectID, midTimestamp)
		assert.NoError(t, err)
		assert.LessOrEqual(t, len(entries), 3, "Should get entries up to timestamp")
		
		// Verify all returned entries are before or at timestamp
		for _, entry := range entries {
			assert.True(t, entry.Timestamp.Before(midTimestamp) || entry.Timestamp.Equal(midTimestamp))
		}
	})
}

func TestBranchService_LSNConsistency(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)

	t.Run("Branch LSN relationships are maintained", func(t *testing.T) {
		// Create main branch
		main, _ := branchService.CreateBranch("lsn-test", "main", "")
		mainCreatedLSN := main.CreatedLSN
		
		// Simulate some operations on main
		for i := 0; i < 3; i++ {
			entry := &wal.Entry{
				ProjectID:  "lsn-test",
				BranchID:   main.ID,
				Operation:  wal.OpInsert,
				Collection: "data",
			}
			lsn, _ := walService.Append(entry)
			branchService.UpdateBranchHead(main.ID, lsn)
		}
		
		// Get updated main
		main, _ = branchService.GetBranchByID(main.ID)
		
		// Create feature branch
		feature, _ := branchService.CreateBranch("lsn-test", "feature", main.ID)
		
		// Verify LSN relationships
		assert.Greater(t, feature.CreatedLSN, main.CreatedLSN, "Feature created after main")
		assert.Equal(t, main.HeadLSN, feature.BaseLSN, "Feature forks at main's HEAD")
		assert.Equal(t, main.HeadLSN, feature.HeadLSN, "Feature starts with main's HEAD")
		assert.Greater(t, main.HeadLSN, mainCreatedLSN, "Main has advanced")
	})
}

func TestWALService_Persistence(t *testing.T) {
	db := setupTestDB(t)
	
	t.Run("LSN survives service restart", func(t *testing.T) {
		// First service instance
		service1, _ := wal.NewService(db)
		
		// Create some entries
		var lastLSN int64
		for i := 0; i < 5; i++ {
			entry := &wal.Entry{
				ProjectID: "persist-test",
				BranchID:  "main",
				Operation: wal.OpInsert,
			}
			lastLSN, _ = service1.Append(entry)
		}
		
		// Create new service instance (simulates restart)
		service2, err := wal.NewService(db)
		assert.NoError(t, err)
		
		// Should continue from where it left off
		assert.Equal(t, lastLSN, service2.GetCurrentLSN())
		
		// New entries should continue sequence
		entry := &wal.Entry{
			ProjectID: "persist-test",
			BranchID:  "main",
			Operation: wal.OpInsert,
		}
		newLSN, _ := service2.Append(entry)
		assert.Equal(t, lastLSN+1, newLSN)
	})
}