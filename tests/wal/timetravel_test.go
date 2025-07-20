package wal_test

import (
	"context"
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
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestTimeTravel_MaterializeAtLSN(t *testing.T) {
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

	// Create project and get main branch
	project, err := projectService.CreateProject("timetravel-test")
	require.NoError(t, err)

	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]

	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Query at different LSNs", func(t *testing.T) {
		// Insert documents at different LSNs
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "u1", "name": "Alice", "version": 1})
		assert.NoError(t, err)
		lsn1 := walService.GetCurrentLSN()

		_, err = interceptor.InsertOne(ctx, "users", bson.M{"_id": "u2", "name": "Bob", "version": 1})
		assert.NoError(t, err)
		lsn2 := walService.GetCurrentLSN()

		// Update first document
		_, err = interceptor.UpdateOne(ctx, "users",
			bson.M{"_id": "u1"},
			bson.M{"$set": bson.M{"version": 2}})
		assert.NoError(t, err)
		lsn3 := walService.GetCurrentLSN()

		// Update branch to get latest HEAD
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Query at LSN1 - only u1 with version 1
		state1, err := timeTravelService.MaterializeAtLSN(branch, "users", lsn1)
		assert.NoError(t, err)
		assert.Len(t, state1, 1)
		assert.Equal(t, int32(1), state1["u1"]["version"])

		// Query at LSN2 - both u1 and u2 with version 1
		state2, err := timeTravelService.MaterializeAtLSN(branch, "users", lsn2)
		assert.NoError(t, err)
		assert.Len(t, state2, 2)
		assert.Equal(t, int32(1), state2["u1"]["version"])
		assert.Equal(t, int32(1), state2["u2"]["version"])

		// Query at LSN3 - u1 updated to version 2
		state3, err := timeTravelService.MaterializeAtLSN(branch, "users", lsn3)
		assert.NoError(t, err)
		assert.Len(t, state3, 2)
		assert.Equal(t, int32(2), state3["u1"]["version"])
		assert.Equal(t, int32(1), state3["u2"]["version"])
	})

	t.Run("Invalid LSN handling", func(t *testing.T) {
		// Negative LSN
		_, err := timeTravelService.MaterializeAtLSN(branch, "users", -1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid LSN")

		// LSN beyond HEAD
		_, err = timeTravelService.MaterializeAtLSN(branch, "users", branch.HeadLSN+100)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "beyond branch HEAD")
	})
}

func TestTimeTravel_MaterializeAtTime(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("time-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Query at different timestamps", func(t *testing.T) {
		// Insert documents with delays
		startTime := time.Now()

		_, err := interceptor.InsertOne(ctx, "events", bson.M{"_id": "e1", "event": "start"})
		assert.NoError(t, err)

		// Small delay to ensure different timestamps
		time.Sleep(10 * time.Millisecond)
		midTime := time.Now()

		_, err = interceptor.InsertOne(ctx, "events", bson.M{"_id": "e2", "event": "middle"})
		assert.NoError(t, err)

		time.Sleep(10 * time.Millisecond)

		_, err = interceptor.InsertOne(ctx, "events", bson.M{"_id": "e3", "event": "end"})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Query at midTime - should see e1 and e2
		state, err := timeTravelService.MaterializeAtTime(branch, "events", midTime.Add(5*time.Millisecond))
		assert.NoError(t, err)
		assert.Len(t, state, 2)
		assert.NotNil(t, state["e1"])
		assert.NotNil(t, state["e2"])
		assert.Nil(t, state["e3"])

		// Query before any events
		_, err = timeTravelService.MaterializeAtTime(branch, "events", startTime.Add(-1*time.Second))
		assert.Error(t, err) // No entries before this time
	})
}

func TestTimeTravel_GetBranchStateAtLSN(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("multi-collection")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Multiple collections at LSN", func(t *testing.T) {
		// Insert into different collections
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "u1", "name": "Alice"})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "products", bson.M{"_id": "p1", "name": "Laptop"})
		assert.NoError(t, err)

		checkpointLSN := walService.GetCurrentLSN()

		_, err = interceptor.InsertOne(ctx, "orders", bson.M{"_id": "o1", "user": "u1", "product": "p1"})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Get state at checkpoint - should have users and products, but not orders
		state, err := timeTravelService.GetBranchStateAtLSN(branch, checkpointLSN)
		assert.NoError(t, err)
		assert.Len(t, state, 2)
		assert.Contains(t, state, "users")
		assert.Contains(t, state, "products")
		assert.NotContains(t, state, "orders")

		// Get current state - should have all three
		currentState, err := timeTravelService.GetBranchStateAtLSN(branch, branch.HeadLSN)
		assert.NoError(t, err)
		assert.Len(t, currentState, 3)
		assert.Contains(t, currentState, "orders")
	})
}

func TestTimeTravel_DocumentHistory(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("doc-history")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Track document changes over time", func(t *testing.T) {
		docID := primitive.NewObjectID()

		// Create document
		_, err := interceptor.InsertOne(ctx, "items", bson.M{
			"_id":     docID,
			"name":    "Item",
			"status":  "draft",
			"version": 1,
		})
		assert.NoError(t, err)
		lsn1 := walService.GetCurrentLSN()

		// Update 1
		_, err = interceptor.UpdateOne(ctx, "items",
			bson.M{"_id": docID},
			bson.M{"$set": bson.M{"status": "review", "version": 2}})
		assert.NoError(t, err)
		lsn2 := walService.GetCurrentLSN()

		// Update 2
		_, err = interceptor.UpdateOne(ctx, "items",
			bson.M{"_id": docID},
			bson.M{"$set": bson.M{"status": "published", "version": 3}})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Get history at different points
		history1, err := timeTravelService.GetDocumentHistoryAtLSN(branch, "items", docID.Hex(), lsn1)
		assert.NoError(t, err)
		assert.Len(t, history1, 1) // Only insert

		history2, err := timeTravelService.GetDocumentHistoryAtLSN(branch, "items", docID.Hex(), lsn2)
		assert.NoError(t, err)
		assert.Len(t, history2, 2) // Insert + first update

		fullHistory, err := timeTravelService.GetDocumentHistoryAtLSN(branch, "items", docID.Hex(), branch.HeadLSN)
		assert.NoError(t, err)
		assert.Len(t, fullHistory, 3) // All operations
	})
}

func TestTimeTravel_ModifiedCollections(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("modified-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Find collections modified in range", func(t *testing.T) {
		startLSN := walService.GetCurrentLSN()

		// Modify various collections
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "u1"})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "products", bson.M{"_id": "p1"})
		assert.NoError(t, err)

		_, err = interceptor.UpdateOne(ctx, "users", bson.M{"_id": "u1"}, bson.M{"$set": bson.M{"updated": true}})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "orders", bson.M{"_id": "o1"})
		assert.NoError(t, err)

		endLSN := walService.GetCurrentLSN()

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Find modified collections
		modified, err := timeTravelService.FindModifiedCollections(branch, startLSN+1, endLSN)
		assert.NoError(t, err)
		assert.Len(t, modified, 3)

		// Verify all three collections are in the list
		modifiedMap := make(map[string]bool)
		for _, col := range modified {
			modifiedMap[col] = true
		}
		assert.True(t, modifiedMap["users"])
		assert.True(t, modifiedMap["products"])
		assert.True(t, modifiedMap["orders"])
	})
}

func TestTimeTravel_Info(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("info-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Get time travel info", func(t *testing.T) {
		// Get info for empty branch
		info, err := timeTravelService.GetTimeTravelInfo(branch)
		assert.NoError(t, err)
		assert.Equal(t, 0, info.EntryCount)

		// Add some data
		_, err = interceptor.InsertOne(ctx, "test", bson.M{"_id": "1"})
		assert.NoError(t, err)
		time.Sleep(10 * time.Millisecond)

		_, err = interceptor.InsertOne(ctx, "test", bson.M{"_id": "2"})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Get updated info
		info, err = timeTravelService.GetTimeTravelInfo(branch)
		assert.NoError(t, err)
		assert.Equal(t, branch.ID, info.BranchID)
		assert.Equal(t, branch.Name, info.BranchName)
		assert.Greater(t, info.EntryCount, 0)
		assert.Greater(t, info.LatestLSN, info.EarliestLSN)
		assert.True(t, info.LatestTime.After(info.EarliestTime))
	})
}

func TestTimeTravel_ComplexScenario(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("complex-scenario")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("E-commerce order lifecycle", func(t *testing.T) {
		// Checkpoint LSNs
		checkpoints := make(map[string]int64)

		// Initial state - add products
		products := []bson.M{
			{"_id": "p1", "name": "Laptop", "price": 1000, "stock": 10},
			{"_id": "p2", "name": "Mouse", "price": 50, "stock": 100},
		}

		for _, p := range products {
			_, err := interceptor.InsertOne(ctx, "products", p)
			assert.NoError(t, err)
		}
		checkpoints["products_added"] = walService.GetCurrentLSN()

		// Customer places order
		_, err := interceptor.InsertOne(ctx, "orders", bson.M{
			"_id":      "order1",
			"customer": "Alice",
			"items": []bson.M{
				{"product": "p1", "quantity": 1},
				{"product": "p2", "quantity": 2},
			},
			"status": "pending",
			"total":  1100,
		})
		assert.NoError(t, err)
		checkpoints["order_placed"] = walService.GetCurrentLSN()

		// Update stock
		_, err = interceptor.UpdateOne(ctx, "products",
			bson.M{"_id": "p1"},
			bson.M{"$inc": bson.M{"stock": -1}})
		assert.NoError(t, err)

		_, err = interceptor.UpdateOne(ctx, "products",
			bson.M{"_id": "p2"},
			bson.M{"$inc": bson.M{"stock": -2}})
		assert.NoError(t, err)
		checkpoints["stock_updated"] = walService.GetCurrentLSN()

		// Process order
		_, err = interceptor.UpdateOne(ctx, "orders",
			bson.M{"_id": "order1"},
			bson.M{"$set": bson.M{"status": "processed", "processed_at": time.Now()}})
		assert.NoError(t, err)
		checkpoints["order_processed"] = walService.GetCurrentLSN()

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Time travel queries

		// 1. State after products added
		state1, err := timeTravelService.GetBranchStateAtLSN(branch, checkpoints["products_added"])
		assert.NoError(t, err)
		assert.Len(t, state1, 1) // Only products
		assert.Equal(t, int32(10), state1["products"]["p1"]["stock"])

		// 2. State after order placed
		state2, err := timeTravelService.GetBranchStateAtLSN(branch, checkpoints["order_placed"])
		assert.NoError(t, err)
		assert.Len(t, state2, 2) // Products and orders
		assert.Equal(t, "pending", state2["orders"]["order1"]["status"])
		assert.Equal(t, int32(10), state2["products"]["p1"]["stock"]) // Stock not updated yet

		// 3. State after stock updated
		state3, err := timeTravelService.GetBranchStateAtLSN(branch, checkpoints["stock_updated"])
		assert.NoError(t, err)
		assert.Equal(t, float64(9), state3["products"]["p1"]["stock"])  // After decrement
		assert.Equal(t, float64(98), state3["products"]["p2"]["stock"]) // After decrement

		// 4. Final state
		state4, err := timeTravelService.GetBranchStateAtLSN(branch, checkpoints["order_processed"])
		assert.NoError(t, err)
		assert.Equal(t, "processed", state4["orders"]["order1"]["status"])
		assert.Contains(t, state4["orders"]["order1"], "processed_at")
	})
}
