package wal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestWriteOperationPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	branch, err := branchService.CreateBranch("perf-test", "main", "")
	require.NoError(t, err)
	
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	ctx := context.Background()

	t.Run("Sequential insert performance", func(t *testing.T) {
		start := time.Now()
		numDocs := 1000
		
		for i := 0; i < numDocs; i++ {
			doc := bson.M{
				"index": i,
				"name":  fmt.Sprintf("User %d", i),
				"email": fmt.Sprintf("user%d@example.com", i),
				"data":  "Some test data for performance testing",
			}
			
			_, err := interceptor.InsertOne(ctx, "users", doc)
			assert.NoError(t, err)
		}
		
		elapsed := time.Since(start)
		opsPerSec := float64(numDocs) / elapsed.Seconds()
		avgLatency := elapsed / time.Duration(numDocs)
		
		t.Logf("Sequential inserts: %d docs in %v", numDocs, elapsed)
		t.Logf("Performance: %.0f ops/sec, avg latency: %v", opsPerSec, avgLatency)
		
		// Should handle at least 500 sequential inserts per second
		assert.Greater(t, opsPerSec, 500.0)
		// Average latency should be under 50ms
		assert.Less(t, avgLatency, 50*time.Millisecond)
	})

	t.Run("Concurrent insert performance", func(t *testing.T) {
		start := time.Now()
		numGoroutines := 10
		docsPerGoroutine := 100
		
		var wg sync.WaitGroup
		errors := make(chan error, numGoroutines*docsPerGoroutine)
		
		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(goroutineID int) {
				defer wg.Done()
				
				for i := 0; i < docsPerGoroutine; i++ {
					doc := bson.M{
						"goroutine": goroutineID,
						"index":     i,
						"name":      fmt.Sprintf("G%d-User%d", goroutineID, i),
					}
					
					_, err := interceptor.InsertOne(ctx, "concurrent_users", doc)
					if err != nil {
						errors <- err
					}
				}
			}(g)
		}
		
		wg.Wait()
		close(errors)
		
		// Check for errors
		var errCount int
		for err := range errors {
			t.Logf("Error during concurrent insert: %v", err)
			errCount++
		}
		assert.Equal(t, 0, errCount, "Should have no errors during concurrent inserts")
		
		elapsed := time.Since(start)
		totalDocs := numGoroutines * docsPerGoroutine
		opsPerSec := float64(totalDocs) / elapsed.Seconds()
		
		t.Logf("Concurrent inserts: %d docs in %v", totalDocs, elapsed)
		t.Logf("Performance: %.0f ops/sec", opsPerSec)
		
		// Should handle at least 2000 concurrent inserts per second
		assert.Greater(t, opsPerSec, 2000.0)
	})

	t.Run("Mixed operations performance", func(t *testing.T) {
		start := time.Now()
		numOps := 300 // 100 of each operation type
		
		// Perform mixed operations
		for i := 0; i < numOps/3; i++ {
			// Insert
			doc := bson.M{"_id": fmt.Sprintf("mixed-%d", i), "value": i}
			_, err := interceptor.InsertOne(ctx, "mixed_ops", doc)
			assert.NoError(t, err)
			
			// Update
			filter := bson.M{"_id": fmt.Sprintf("mixed-%d", i)}
			update := bson.M{"$set": bson.M{"value": i * 2}}
			_, err = interceptor.UpdateOne(ctx, "mixed_ops", filter, update)
			assert.NoError(t, err)
			
			// Delete every other document
			if i%2 == 0 {
				_, err = interceptor.DeleteOne(ctx, "mixed_ops", filter)
				assert.NoError(t, err)
			}
		}
		
		elapsed := time.Since(start)
		opsPerSec := float64(numOps) / elapsed.Seconds()
		
		t.Logf("Mixed operations: %d ops in %v", numOps, elapsed)
		t.Logf("Performance: %.0f ops/sec", opsPerSec)
		
		// Should handle at least 300 mixed operations per second
		assert.Greater(t, opsPerSec, 300.0)
	})

	t.Run("Large document performance", func(t *testing.T) {
		// Create a large document (1MB)
		largeData := make([]byte, 1024*1024)
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}
		
		largeDoc := bson.M{
			"name": "Large Document",
			"data": largeData,
			"metadata": bson.M{
				"size":      len(largeData),
				"timestamp": time.Now(),
			},
		}
		
		start := time.Now()
		result, err := interceptor.InsertOne(ctx, "large_docs", largeDoc)
		elapsed := time.Since(start)
		
		assert.NoError(t, err)
		assert.NotNil(t, result.InsertedID)
		
		t.Logf("Large document (1MB) insert took: %v", elapsed)
		// Large document should still be inserted within 100ms
		assert.Less(t, elapsed, 100*time.Millisecond)
	})
}

func setupBenchDB(b *testing.B) *mongo.Database {
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(b, err)

	dbName := "argon_wal_bench_" + time.Now().Format("20060102150405")
	db := client.Database(dbName)

	b.Cleanup(func() {
		db.Drop(context.Background())
		client.Disconnect(context.Background())
	})

	return db
}

func BenchmarkWALWrites(b *testing.B) {
	db := setupBenchDB(b)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	branch, _ := branchService.CreateBranch("bench-test", "main", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	ctx := context.Background()

	b.Run("InsertOne", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			doc := bson.M{
				"index": i,
				"name":  fmt.Sprintf("User %d", i),
			}
			interceptor.InsertOne(ctx, "bench_users", doc)
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	})

	b.Run("UpdateOne", func(b *testing.B) {
		// Pre-insert documents
		for i := 0; i < 100; i++ {
			doc := bson.M{"_id": fmt.Sprintf("update-%d", i), "value": 0}
			interceptor.InsertOne(ctx, "bench_updates", doc)
		}
		
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			filter := bson.M{"_id": fmt.Sprintf("update-%d", i%100)}
			update := bson.M{"$set": bson.M{"value": i}}
			interceptor.UpdateOne(ctx, "bench_updates", filter, update)
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	})
}