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
				Operation:  wal.OpPut,
				Collection: "test",
				DocumentID: fmt.Sprintf("doc-%d", i),
				PostImage: mustMarshalBSON(map[string]interface{}{
					"_id":   fmt.Sprintf("doc-%d", i),
					"index": i,
					"data":  "test data for performance testing",
				}),
			}
			_, err := walService.Append(entry)
			assert.NoError(t, err)
		}

		elapsed := time.Since(start)
		opsPerSec := float64(numOps) / elapsed.Seconds()

		t.Logf("Appended %d entries in %v (%.0f ops/sec)", numOps, elapsed, opsPerSec)
		// Regression canary, not a benchmark: every append is a sequencer
		// reservation plus an insert, so this floor is dominated by driver
		// round-trip latency, which varies wildly between local Docker and
		// CI. High-throughput writers should use AppendBatch, which pays
		// for the sequencer once per batch.
		performanceGreater(t, opsPerSec, 150.0, "Should handle at least 150 sequential ops/sec")
		entries, err := walService.GetBranchEntries("main", "test", 0, walService.GetCurrentLSN("perf-test"))
		require.NoError(t, err)
		require.Len(t, entries, numOps)
		for i, entry := range entries {
			require.Equal(t, int64(i+1), entry.LSN)
		}
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
					docID := fmt.Sprintf("g%d-doc-%d", goroutineID, i)
					entry := &wal.Entry{
						ProjectID:  "concurrent-perf",
						BranchID:   fmt.Sprintf("branch-%d", goroutineID),
						Operation:  wal.OpPut,
						Collection: "test",
						DocumentID: docID,
						PostImage:  mustMarshalBSON(map[string]interface{}{"_id": docID}),
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
		// Regression canary; see the note on the sequential floor.
		performanceGreater(t, opsPerSec, 500.0, "Should handle at least 500 concurrent ops/sec")
		seen := make(map[int64]bool, totalOps)
		for g := 0; g < numGoroutines; g++ {
			entries, err := walService.GetBranchEntries(fmt.Sprintf("branch-%d", g), "test", 0, walService.GetCurrentLSN("concurrent-perf"))
			require.NoError(t, err)
			require.Len(t, entries, opsPerGoroutine)
			for _, entry := range entries {
				require.False(t, seen[entry.LSN], "concurrent reservations must never duplicate LSNs")
				seen[entry.LSN] = true
			}
		}
		require.Len(t, seen, totalOps)
	})

	t.Run("Query performance", func(t *testing.T) {
		// Query the entries we just created
		start := time.Now()

		entries, err := walService.GetBranchEntries("main", "test", 0, walService.GetCurrentLSN("perf-test"))
		assert.NoError(t, err)

		elapsed := time.Since(start)
		t.Logf("Retrieved %d entries in %v", len(entries), elapsed)
		performanceLess(t, elapsed, 500*time.Millisecond, "Query should complete within 500ms")
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
		performanceLess(t, avgTime, 10*time.Millisecond, "Branch creation should be under 10ms")
		children, err := branchService.GetChildBranches(main.ID)
		require.NoError(t, err)
		require.Len(t, children, numBranches)
		for _, child := range children {
			require.Equal(t, main.HeadLSN, child.BaseLSN)
			require.Equal(t, child.BaseLSN, child.HeadLSN)
		}
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
		performanceLess(t, avgTime, 20*time.Millisecond, "Hierarchical branch creation should be under 20ms")
		for i := 0; i < depth; i++ {
			branch, err := branchService.GetBranchByID(parentID)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("level-%d", depth-i-1), branch.Name)
			parentID = branch.ParentID
		}
		require.Empty(t, parentID)
	})
}
