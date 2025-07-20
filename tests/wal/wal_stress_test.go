package wal_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWALPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	t.Run("Append performance", func(t *testing.T) {
		start := time.Now()
		numOps := 10000

		for i := 0; i < numOps; i++ {
			entry := &wal.Entry{
				ProjectID:  "perf-test",
				BranchID:   "main",
				Operation:  wal.OpInsert,
				Collection: "test",
				DocumentID: fmt.Sprintf("doc-%d", i),
				Document:   mustMarshalBSON(map[string]interface{}{
					"_id": fmt.Sprintf("doc-%d", i),
					"index": i,
					"data": "test data for performance testing",
				}),
			}
			_, err := walService.Append(entry)
			assert.NoError(t, err)
		}

		elapsed := time.Since(start)
		opsPerSec := float64(numOps) / elapsed.Seconds()
		
		t.Logf("Appended %d entries in %v (%.0f ops/sec)", numOps, elapsed, opsPerSec)
		assert.Greater(t, opsPerSec, 1000.0, "Should handle at least 1000 ops/sec")
	})

	t.Run("Concurrent append performance", func(t *testing.T) {
		start := time.Now()
		numGoroutines := 10
		opsPerGoroutine := 1000
		
		var wg sync.WaitGroup
		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(goroutineID int) {
				defer wg.Done()
				
				for i := 0; i < opsPerGoroutine; i++ {
					entry := &wal.Entry{
						ProjectID:  "concurrent-perf",
						BranchID:   fmt.Sprintf("branch-%d", goroutineID),
						Operation:  wal.OpInsert,
						Collection: "test",
						DocumentID: fmt.Sprintf("g%d-doc-%d", goroutineID, i),
					}
					_, err := walService.Append(entry)
					assert.NoError(t, err)
				}
			}(g)
		}
		
		wg.Wait()
		elapsed := time.Since(start)
		totalOps := numGoroutines * opsPerGoroutine
		opsPerSec := float64(totalOps) / elapsed.Seconds()
		
		t.Logf("Concurrent: %d ops in %v (%.0f ops/sec)", totalOps, elapsed, opsPerSec)
		assert.Greater(t, opsPerSec, 5000.0, "Should handle at least 5000 concurrent ops/sec")
	})

	t.Run("Query performance", func(t *testing.T) {
		// Query the entries we just created
		start := time.Now()
		
		entries, err := walService.GetBranchEntries("main", "test", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		
		elapsed := time.Since(start)
		t.Logf("Retrieved %d entries in %v", len(entries), elapsed)
		assert.Less(t, elapsed, 500*time.Millisecond, "Query should complete within 500ms")
	})
}

func TestBranchPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	t.Run("Branch creation performance", func(t *testing.T) {
		// Create main branch
		main, err := branchService.CreateBranch("perf-project", "main", "")
		require.NoError(t, err)

		start := time.Now()
		numBranches := 100

		for i := 0; i < numBranches; i++ {
			branchName := fmt.Sprintf("feature-%d", i)
			_, err := branchService.CreateBranch("perf-project", branchName, main.ID)
			assert.NoError(t, err)
		}

		elapsed := time.Since(start)
		avgTime := elapsed / time.Duration(numBranches)
		
		t.Logf("Created %d branches in %v (avg: %v per branch)", numBranches, elapsed, avgTime)
		assert.Less(t, avgTime, 10*time.Millisecond, "Branch creation should be under 10ms")
	})

	t.Run("Branch hierarchy performance", func(t *testing.T) {
		// Create deep branch hierarchy
		projectID := "hierarchy-test"
		parentID := ""
		depth := 50
		
		start := time.Now()
		
		for i := 0; i < depth; i++ {
			branchName := fmt.Sprintf("level-%d", i)
			branch, err := branchService.CreateBranch(projectID, branchName, parentID)
			assert.NoError(t, err)
			parentID = branch.ID
		}
		
		elapsed := time.Since(start)
		avgTime := elapsed / time.Duration(depth)
		
		t.Logf("Created %d-level hierarchy in %v (avg: %v per level)", depth, elapsed, avgTime)
		assert.Less(t, avgTime, 20*time.Millisecond, "Hierarchical branch creation should be under 20ms")
	})
}