package wal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
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

	mat := materializer.NewService(walService, branchService)
	writer := walwriter.New(walService, branchService, mat, branch)
	ctx := context.Background()

	t.Run("Sequential put performance", func(t *testing.T) {
		start := time.Now()
		numDocs := 1000

		for i := 0; i < numDocs; i++ {
			_, err := writer.Put(ctx, "users", bson.M{
				"_id":   fmt.Sprintf("user-%d", i),
				"name":  fmt.Sprintf("User %d", i),
				"email": fmt.Sprintf("user%d@example.com", i),
				"data":  "Some test data for performance testing",
			})
			assert.NoError(t, err)
		}

		elapsed := time.Since(start)
		opsPerSec := float64(numDocs) / elapsed.Seconds()
		avgLatency := elapsed / time.Duration(numDocs)

		t.Logf("Sequential puts: %d docs in %v", numDocs, elapsed)
		t.Logf("Performance: %.0f ops/sec, avg latency: %v", opsPerSec, avgLatency)

		// These floors are canaries against gross regressions, not
		// benchmarks: absolute numbers here are dominated by driver
		// round-trip latency (each put is a pre-image lookup, a sequencer
		// reservation, an entry insert and a branch-head update), which
		// varies wildly between local Docker and CI.
		assert.Greater(t, opsPerSec, 100.0)
		assert.Less(t, avgLatency, 50*time.Millisecond)
	})

	t.Run("Batched put performance", func(t *testing.T) {
		start := time.Now()
		numDocs := 5000
		batchSize := 500

		for b := 0; b < numDocs/batchSize; b++ {
			batch := make([]bson.M, batchSize)
			for i := range batch {
				batch[i] = bson.M{
					"_id":  fmt.Sprintf("batch-%d-%d", b, i),
					"name": fmt.Sprintf("Batch user %d", i),
				}
			}
			_, err := writer.PutMany(ctx, "batched", batch)
			assert.NoError(t, err)
		}

		elapsed := time.Since(start)
		opsPerSec := float64(numDocs) / elapsed.Seconds()
		t.Logf("Batched puts: %d docs in %v (%.0f ops/sec)", numDocs, elapsed, opsPerSec)
		assert.Greater(t, opsPerSec, 500.0, "batching amortizes the per-write round-trips")
	})

	t.Run("Concurrent put performance", func(t *testing.T) {
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
					_, err := writer.Put(ctx, "concurrent_users", bson.M{
						"_id":  fmt.Sprintf("g%d-user%d", goroutineID, i),
						"name": fmt.Sprintf("G%d-User%d", goroutineID, i),
					})
					if err != nil {
						errors <- err
					}
				}
			}(g)
		}

		wg.Wait()
		close(errors)

		var errCount int
		for err := range errors {
			t.Logf("Error during concurrent put: %v", err)
			errCount++
		}
		assert.Equal(t, 0, errCount, "Should have no errors during concurrent puts")

		elapsed := time.Since(start)
		totalDocs := numGoroutines * docsPerGoroutine
		opsPerSec := float64(totalDocs) / elapsed.Seconds()
		t.Logf("Concurrent puts: %d docs in %v (%.0f ops/sec)", totalDocs, elapsed, opsPerSec)
		// Regression canary; see the note on the sequential floor.
		assert.Greater(t, opsPerSec, 500.0)
	})

	t.Run("Large document performance", func(t *testing.T) {
		largeData := make([]byte, 1024*1024)
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}

		start := time.Now()
		_, err := writer.Put(ctx, "large_docs", bson.M{
			"_id":  "large",
			"name": "Large Document",
			"data": largeData,
		})
		elapsed := time.Since(start)

		assert.NoError(t, err)
		t.Logf("Large document (1MB) put took: %v", elapsed)
		assert.Less(t, elapsed, 100*time.Millisecond)
	})
}

func setupBenchDB(b *testing.B) *mongo.Database {
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(b, err)

	dbName := fmt.Sprintf("argon_wal_bench_%d", time.Now().UnixNano())
	db := client.Database(dbName)

	b.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})

	return db
}

func BenchmarkWALWrites(b *testing.B) {
	db := setupBenchDB(b)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	branch, _ := branchService.CreateBranch("bench-test", "main", "")
	mat := materializer.NewService(walService, branchService)
	writer := walwriter.New(walService, branchService, mat, branch)
	ctx := context.Background()

	b.Run("Put", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = writer.Put(ctx, "bench_users", bson.M{
				"_id":  fmt.Sprintf("user-%d", i),
				"name": fmt.Sprintf("User %d", i),
			})
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	})

	b.Run("Overwrite", func(b *testing.B) {
		for i := 0; i < 100; i++ {
			_, _ = writer.Put(ctx, "bench_updates", bson.M{"_id": fmt.Sprintf("update-%d", i), "value": 0})
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = writer.Put(ctx, "bench_updates", bson.M{"_id": fmt.Sprintf("update-%d", i%100), "value": i})
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/sec")
	})
}
