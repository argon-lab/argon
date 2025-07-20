package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestTimeTravelPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)
	
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()
	
	project, _ := projectService.CreateProject("perf-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	t.Run("Time travel query performance", func(t *testing.T) {
		// Create a significant history
		numOperations := 1000
		checkpoints := make([]int64, 0, 10)
		
		// Insert and update documents
		for i := 0; i < numOperations; i++ {
			if i%100 == 0 {
				// Insert new document every 100 operations
				doc := bson.M{
					"_id": fmt.Sprintf("doc%d", i/100),
					"index": i,
					"data": fmt.Sprintf("Initial data for document %d", i/100),
					"timestamp": time.Now(),
				}
				_, err := interceptor.InsertOne(ctx, "history", doc)
				assert.NoError(t, err)
			} else {
				// Update existing documents
				docID := fmt.Sprintf("doc%d", i/100)
				update := bson.M{
					"$set": bson.M{
						"index": i,
						"data": fmt.Sprintf("Updated data at operation %d", i),
						"timestamp": time.Now(),
					},
				}
				_, err := interceptor.UpdateOne(ctx, "history", bson.M{"_id": docID}, update)
				assert.NoError(t, err)
			}
			
			// Save checkpoints every 100 operations
			if i%100 == 99 {
				checkpoints = append(checkpoints, walService.GetCurrentLSN())
			}
		}
		
		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Measure time travel query performance at different points
		for i, checkpoint := range checkpoints {
			start := time.Now()
			state, err := timeTravelService.MaterializeAtLSN(branch, "history", checkpoint)
			elapsed := time.Since(start)
			
			assert.NoError(t, err)
			expectedDocs := (i + 1) // One doc per 100 operations
			assert.Len(t, state, expectedDocs)
			
			t.Logf("Time travel to checkpoint %d (LSN %d): %v for %d docs", i, checkpoint, elapsed, expectedDocs)
			
			// Should complete within 500ms even for large histories
			assert.Less(t, elapsed, 500*time.Millisecond)
		}
	})
	
	t.Run("Concurrent time travel queries", func(t *testing.T) {
		// Create some data
		for i := 0; i < 100; i++ {
			doc := bson.M{
				"_id": fmt.Sprintf("concurrent%d", i),
				"value": i,
			}
			_, err := interceptor.InsertOne(ctx, "concurrent", doc)
			assert.NoError(t, err)
		}
		
		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)
		targetLSN := branch.HeadLSN - 50 // Query at a mid-point
		
		// Run concurrent queries
		numGoroutines := 10
		iterations := 100
		
		start := time.Now()
		done := make(chan bool, numGoroutines)
		
		for g := 0; g < numGoroutines; g++ {
			go func(goroutineID int) {
				for i := 0; i < iterations; i++ {
					state, err := timeTravelService.MaterializeAtLSN(branch, "concurrent", targetLSN)
					assert.NoError(t, err)
					assert.NotEmpty(t, state)
				}
				done <- true
			}(g)
		}
		
		// Wait for all goroutines
		for i := 0; i < numGoroutines; i++ {
			<-done
		}
		
		elapsed := time.Since(start)
		totalQueries := numGoroutines * iterations
		qps := float64(totalQueries) / elapsed.Seconds()
		avgLatency := elapsed / time.Duration(totalQueries)
		
		t.Logf("Concurrent time travel: %d queries in %v", totalQueries, elapsed)
		t.Logf("Performance: %.0f queries/sec, avg latency: %v", qps, avgLatency)
		
		// Should handle at least 1000 queries per second
		assert.Greater(t, qps, 1000.0)
	})
	
	t.Run("Time-based query performance", func(t *testing.T) {
		// Create documents with known timestamps
		timestamps := make([]time.Time, 0)
		baseTime := time.Now()
		
		for i := 0; i < 50; i++ {
			timestamp := baseTime.Add(time.Duration(i) * time.Second)
			doc := bson.M{
				"_id": fmt.Sprintf("timed%d", i),
				"created_at": timestamp,
				"sequence": i,
			}
			_, err := interceptor.InsertOne(ctx, "timed", doc)
			assert.NoError(t, err)
			
			if i%10 == 0 {
				timestamps = append(timestamps, timestamp.Add(500*time.Millisecond))
			}
		}
		
		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Query at different timestamps
		for _, ts := range timestamps {
			start := time.Now()
			state, err := timeTravelService.MaterializeAtTime(branch, "timed", ts)
			elapsed := time.Since(start)
			
			if err != nil {
				t.Logf("Error at timestamp %v: %v", ts, err)
				continue
			}
			
			t.Logf("Time travel to %v: %v for %d docs", ts.Format("15:04:05"), elapsed, len(state))
			
			// Time-based queries should also be fast
			assert.Less(t, elapsed, 200*time.Millisecond)
		}
	})
	
	t.Run("Large collection time travel", func(t *testing.T) {
		// Create a large collection
		numDocs := 5000
		batchSize := 100
		
		for batch := 0; batch < numDocs/batchSize; batch++ {
			for i := 0; i < batchSize; i++ {
				doc := bson.M{
					"_id": fmt.Sprintf("large%d", batch*batchSize+i),
					"batch": batch,
					"index": i,
					"data": "Some test data for the document",
				}
				_, err := interceptor.InsertOne(ctx, "large", doc)
				assert.NoError(t, err)
			}
		}
		
		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Measure materialization time for the full collection
		start := time.Now()
		state, err := timeTravelService.MaterializeAtLSN(branch, "large", branch.HeadLSN)
		elapsed := time.Since(start)
		
		assert.NoError(t, err)
		assert.Len(t, state, numDocs)
		
		docsPerSec := float64(numDocs) / elapsed.Seconds()
		t.Logf("Materialized %d documents in %v (%.0f docs/sec)", numDocs, elapsed, docsPerSec)
		
		// Should materialize at least 10,000 docs per second
		assert.Greater(t, docsPerSec, 10000.0)
	})
}

func BenchmarkTimeTravel(b *testing.B) {
	db := setupBenchDB(b)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()
	
	project, _ := projectService.CreateProject("bench")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	// Create test data
	for i := 0; i < 1000; i++ {
		doc := bson.M{"_id": fmt.Sprintf("bench%d", i), "value": i}
		interceptor.InsertOne(ctx, "bench", doc)
	}
	
	branch, _ = branchService.GetBranchByID(branch.ID)
	targetLSN := branch.HeadLSN / 2
	
	b.Run("MaterializeAtLSN", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := timeTravelService.MaterializeAtLSN(branch, "bench", targetLSN)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "queries/sec")
	})
	
	b.Run("GetBranchStateAtLSN", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := timeTravelService.GetBranchStateAtLSN(branch, targetLSN)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "queries/sec")
	})
}