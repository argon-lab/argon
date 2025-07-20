package wal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWeek2_ComprehensiveIntegration(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)

	materializerService := materializer.NewService(walService)
	ctx := context.Background()

	t.Run("Complete workflow test", func(t *testing.T) {
		// 1. Create project
		project, err := projectService.CreateProject("test-app")
		require.NoError(t, err)

		// Get main branch
		branches, err := branchService.ListBranches(project.ID)
		require.NoError(t, err)
		mainBranch := branches[0]

		// 2. Test write operations
		interceptor := driverwal.NewInterceptor(walService, mainBranch, branchService)

		// Insert users
		users := []bson.M{
			{"_id": "u1", "name": "Alice", "role": "admin", "active": true},
			{"_id": "u2", "name": "Bob", "role": "user", "active": true},
			{"_id": "u3", "name": "Charlie", "role": "user", "active": false},
		}

		for _, user := range users {
			_, err := interceptor.InsertOne(ctx, "users", user)
			assert.NoError(t, err)
		}

		// Update operations
		_, err = interceptor.UpdateOne(ctx, "users",
			bson.M{"_id": "u2"},
			bson.M{"$set": bson.M{"role": "moderator", "updated_at": time.Now()}})
		assert.NoError(t, err)

		// Delete operation
		_, err = interceptor.DeleteOne(ctx, "users", bson.M{"_id": "u3"})
		assert.NoError(t, err)

		// 3. Test materialization
		mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)
		state, err := materializerService.MaterializeCollection(mainBranch, "users")
		assert.NoError(t, err)
		assert.Len(t, state, 2) // u3 was deleted

		// Verify updates
		assert.Equal(t, "moderator", state["u2"]["role"])
		assert.Contains(t, state["u2"], "updated_at")

		// 4. Test query operations
		collection := driverwal.NewCollection("users", mainBranch, walService, branchService, materializerService, nil)

		// Count all
		count, err := collection.CountDocuments(ctx, bson.M{})
		assert.NoError(t, err)
		assert.Equal(t, int64(2), count)

		// Count with filter
		count, err = collection.CountDocuments(ctx, bson.M{"role": "admin"})
		assert.NoError(t, err)
		assert.Equal(t, int64(1), count)

		// Find one
		result := collection.FindOne(ctx, bson.M{"_id": "u1"})
		var user bson.M
		err = result.Decode(&user)
		assert.NoError(t, err)
		assert.Equal(t, "Alice", user["name"])

		// 5. Test branch isolation
		featureBranch, err := branchService.CreateBranch(project.ID, "feature", mainBranch.ID)
		require.NoError(t, err)

		featureInterceptor := driverwal.NewInterceptor(walService, featureBranch, branchService)

		// Insert a new user in feature branch
		_, err = featureInterceptor.InsertOne(ctx, "users", bson.M{
			"_id":  "u4",
			"name": "David",
			"role": "developer",
		})
		assert.NoError(t, err)

		// Update a user in feature branch (note: in MVP, branches are isolated)
		_, err = featureInterceptor.InsertOne(ctx, "users", bson.M{
			"_id":  "u1",
			"name": "Alice",
			"role": "superadmin",
		})
		assert.NoError(t, err)

		// Verify isolation
		mainState, _ := materializerService.MaterializeCollection(mainBranch, "users")
		featureBranch, _ = branchService.GetBranchByID(featureBranch.ID)
		featureState, _ := materializerService.MaterializeCollection(featureBranch, "users")

		// Main branch unchanged
		assert.Equal(t, "admin", mainState["u1"]["role"])
		assert.Nil(t, mainState["u4"]) // u4 doesn't exist in main

		// Feature branch has its own data
		assert.Equal(t, "superadmin", featureState["u1"]["role"])
		assert.Equal(t, "developer", featureState["u4"]["role"])
	})

	t.Run("Complex update operators", func(t *testing.T) {
		project, _ := projectService.CreateProject("analytics")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		// Insert document with nested fields
		doc := bson.M{
			"_id": "stats1",
			"metrics": bson.M{
				"views":       1000,
				"clicks":      50,
				"conversions": 5,
			},
			"tags": []string{"featured", "trending"},
			"settings": bson.M{
				"notifications": true,
				"theme":         "dark",
			},
		}

		_, err := interceptor.InsertOne(ctx, "analytics", doc)
		assert.NoError(t, err)

		// Test $inc with nested fields
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": "stats1"},
			bson.M{"$inc": bson.M{
				"metrics.views":  100,
				"metrics.clicks": 10,
			}})
		assert.NoError(t, err)

		// Test $unset
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": "stats1"},
			bson.M{"$unset": bson.M{"tags": ""}})
		assert.NoError(t, err)

		// Test $set with nested
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": "stats1"},
			bson.M{"$set": bson.M{"settings.theme": "light"}})
		assert.NoError(t, err)

		// Verify all updates
		branch, _ = branchService.GetBranchByID(branch.ID)
		state, _ := materializerService.MaterializeCollection(branch, "analytics")

		metrics := state["stats1"]["metrics"].(bson.M)
		// The materializer's addNumbers returns float64
		assert.Equal(t, float64(1100), metrics["views"])
		assert.Equal(t, float64(60), metrics["clicks"])

		assert.NotContains(t, state["stats1"], "tags")

		settings := state["stats1"]["settings"].(bson.M)
		assert.Equal(t, "light", settings["theme"])
	})

	t.Run("Query operators comprehensive test", func(t *testing.T) {
		project, _ := projectService.CreateProject("inventory")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		// Insert test data
		products := []bson.M{
			{"_id": "p1", "name": "Laptop", "price": 999.99, "stock": 50, "category": "Electronics"},
			{"_id": "p2", "name": "Mouse", "price": 29.99, "stock": 200, "category": "Accessories"},
			{"_id": "p3", "name": "Keyboard", "price": 79.99, "stock": 150, "category": "Accessories"},
			{"_id": "p4", "name": "Monitor", "price": 299.99, "stock": 75, "category": "Electronics"},
			{"_id": "p5", "name": "Cable", "price": 9.99, "stock": 500, "category": "Accessories"},
		}

		for _, product := range products {
			_, err := interceptor.InsertOne(ctx, "products", product)
			assert.NoError(t, err)
		}

		branch, _ = branchService.GetBranchByID(branch.ID)
		collection := driverwal.NewCollection("products", branch, walService, branchService, materializerService, nil)

		// Test various operators
		testCases := []struct {
			name     string
			filter   bson.M
			expected int64
		}{
			{"$gt operator", bson.M{"price": bson.M{"$gt": 100}}, 2},
			{"$lte operator", bson.M{"price": bson.M{"$lte": 30}}, 2},
			{"$ne operator", bson.M{"category": bson.M{"$ne": "Electronics"}}, 3},
			{"$in operator", bson.M{"_id": bson.M{"$in": []string{"p1", "p3", "p5"}}}, 3},
			{"Combined filters", bson.M{
				"category": "Accessories",
				"price":    bson.M{"$lt": 50},
			}, 2},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				count, err := collection.CountDocuments(ctx, tc.filter)
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, count, "Filter: %v", tc.filter)
			})
		}
	})

	t.Run("Concurrent operations stress test", func(t *testing.T) {
		project, _ := projectService.CreateProject("stress-test")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]

		numGoroutines := 10
		opsPerGoroutine := 50
		var wg sync.WaitGroup
		errors := make(chan error, numGoroutines*opsPerGoroutine)

		start := time.Now()

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(goroutineID int) {
				defer wg.Done()
				interceptor := driverwal.NewInterceptor(walService, branch, branchService)

				for i := 0; i < opsPerGoroutine; i++ {
					// Mix of operations
					switch i % 3 {
					case 0: // Insert
						doc := bson.M{
							"_id":       fmt.Sprintf("g%d-i%d", goroutineID, i),
							"goroutine": goroutineID,
							"index":     i,
							"timestamp": time.Now(),
						}
						_, err := interceptor.InsertOne(ctx, "stress", doc)
						if err != nil {
							errors <- err
						}
					case 1: // Update
						if i > 0 {
							filter := bson.M{"_id": fmt.Sprintf("g%d-i%d", goroutineID, i-1)}
							update := bson.M{"$set": bson.M{"updated": true, "update_count": i}}
							_, err := interceptor.UpdateOne(ctx, "stress", filter, update)
							if err != nil {
								errors <- err
							}
						}
					case 2: // Delete
						if i > 2 {
							filter := bson.M{"_id": fmt.Sprintf("g%d-i%d", goroutineID, i-2)}
							_, err := interceptor.DeleteOne(ctx, "stress", filter)
							if err != nil {
								errors <- err
							}
						}
					}
				}
			}(g)
		}

		wg.Wait()
		close(errors)

		elapsed := time.Since(start)
		totalOps := numGoroutines * opsPerGoroutine
		opsPerSec := float64(totalOps) / elapsed.Seconds()

		// Check for errors
		var errCount int
		for err := range errors {
			t.Logf("Error: %v", err)
			errCount++
		}
		assert.Equal(t, 0, errCount)

		t.Logf("Concurrent stress test: %d operations in %v (%.0f ops/sec)", totalOps, elapsed, opsPerSec)
		assert.Greater(t, opsPerSec, 1000.0) // Should handle > 1000 ops/sec

		// Verify final state consistency
		branch, _ = branchService.GetBranchByID(branch.ID)
		state, err := materializerService.MaterializeCollection(branch, "stress")
		assert.NoError(t, err)
		t.Logf("Final state has %d documents", len(state))
	})

	t.Run("Document history tracking", func(t *testing.T) {
		project, _ := projectService.CreateProject("history-test")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		docID := primitive.NewObjectID()

		// Create document
		_, err := interceptor.InsertOne(ctx, "items", bson.M{
			"_id":     docID,
			"name":    "Test Item",
			"status":  "draft",
			"version": 1,
		})
		assert.NoError(t, err)

		// Update multiple times
		statuses := []string{"review", "approved", "published"}
		for i, status := range statuses {
			_, err = interceptor.UpdateOne(ctx, "items",
				bson.M{"_id": docID},
				bson.M{"$set": bson.M{
					"status":     status,
					"version":    i + 2,
					"updated_at": time.Now(),
				}})
			assert.NoError(t, err)
		}

		// Get document history
		branch, _ = branchService.GetBranchByID(branch.ID)
		entries, err := walService.GetDocumentHistory(branch.ID, "items", docID.Hex(), 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		assert.Len(t, entries, 4) // 1 insert + 3 updates

		// Verify final state
		finalDoc, err := materializerService.MaterializeDocument(branch, "items", docID.Hex())
		assert.NoError(t, err)
		assert.Equal(t, "published", finalDoc["status"])
		assert.Equal(t, int32(4), finalDoc["version"])
	})
}

func TestWeek2_EdgeCases(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	ctx := context.Background()

	t.Run("Empty filter matches all", func(t *testing.T) {
		project, _ := projectService.CreateProject("edge-test")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		// Insert test docs
		for i := 0; i < 5; i++ {
			_, err := interceptor.InsertOne(ctx, "test", bson.M{"_id": i, "value": i * 10})
			assert.NoError(t, err)
		}

		branch, _ = branchService.GetBranchByID(branch.ID)
		collection := driverwal.NewCollection("test", branch, walService, branchService, materializerService, nil)

		count, err := collection.CountDocuments(ctx, bson.M{})
		assert.NoError(t, err)
		assert.Equal(t, int64(5), count)
	})

	t.Run("Update non-existent document", func(t *testing.T) {
		project, _ := projectService.CreateProject("edge-test2")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		// Update document that doesn't exist
		result, err := interceptor.UpdateOne(ctx, "test",
			bson.M{"_id": "nonexistent"},
			bson.M{"$set": bson.M{"value": 100}})
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.MatchedCount) // MVP assumes match

		// Verify no document created
		branch, _ = branchService.GetBranchByID(branch.ID)
		state, _ := materializerService.MaterializeCollection(branch, "test")
		assert.Empty(t, state)
	})

	t.Run("Delete with complex filter", func(t *testing.T) {
		project, _ := projectService.CreateProject("edge-test3")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)

		// Insert docs
		docs := []bson.M{
			{"_id": "1", "type": "A", "value": 10},
			{"_id": "2", "type": "B", "value": 20},
			{"_id": "3", "type": "A", "value": 30},
		}

		for _, doc := range docs {
			_, err := interceptor.InsertOne(ctx, "test", doc)
			assert.NoError(t, err)
		}

		// Delete with non-ID filter (no document ID extraction)
		result, err := interceptor.DeleteOne(ctx, "test", bson.M{"type": "A"})
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.DeletedCount)

		// Verify WAL entry created without document ID
		entries, _ := walService.GetBranchEntries(branch.ID, "test", 0, walService.GetCurrentLSN())
		var deleteEntry *wal.Entry
		for _, entry := range entries {
			if entry.Operation == wal.OpDelete {
				deleteEntry = entry
				break
			}
		}
		assert.NotNil(t, deleteEntry)
		assert.Empty(t, deleteEntry.DocumentID) // No ID extracted from complex filter
	})
}
