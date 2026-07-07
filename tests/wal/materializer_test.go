package wal_test

import (
	"context"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestMaterializer_MaterializeCollection(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	materializerService := materializer.NewService(walService, branchService)

	// Create test branch
	branch, err := branchService.CreateBranch("test-project", "test-branch", "")
	require.NoError(t, err)

	// Create interceptor for writing
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, materializerService)
	ctx := context.Background()

	t.Run("Materialize empty collection", func(t *testing.T) {
		state, err := materializerService.MaterializeCollection(branch, "empty_collection")
		assert.NoError(t, err)
		assert.Empty(t, state)
	})

	t.Run("Materialize collection with inserts", func(t *testing.T) {
		docs := []bson.M{
			{"_id": "1", "name": "Alice", "age": 30},
			{"_id": "2", "name": "Bob", "age": 25},
			{"_id": "3", "name": "Charlie", "age": 35},
		}

		for _, doc := range docs {
			_, err := interceptor.InsertOne(ctx, "users", doc)
			assert.NoError(t, err)
		}

		branch, _ = branchService.GetBranchByID(branch.ID)

		state, err := materializerService.MaterializeCollection(branch, "users")
		assert.NoError(t, err)
		assert.Len(t, state, 3)

		assert.Equal(t, "Alice", state["1"]["name"])
		assert.Equal(t, "Bob", state["2"]["name"])
		assert.Equal(t, "Charlie", state["3"]["name"])
	})

	t.Run("Materialize with updates", func(t *testing.T) {
		filter := bson.M{"name": "Alice"}
		update := bson.M{"$set": bson.M{"age": 31, "city": "New York"}}
		_, err := interceptor.UpdateOne(ctx, "users", filter, update, false)
		assert.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)

		state, err := materializerService.MaterializeCollection(branch, "users")
		assert.NoError(t, err)

		assert.EqualValues(t, 31, state["1"]["age"])
		assert.Equal(t, "New York", state["1"]["city"])
	})

	t.Run("Materialize with deletes", func(t *testing.T) {
		filter := bson.M{"name": "Bob"}
		_, err := interceptor.DeleteOne(ctx, "users", filter)
		assert.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)

		state, err := materializerService.MaterializeCollection(branch, "users")
		assert.NoError(t, err)

		assert.Len(t, state, 2)
		assert.NotContains(t, state, "2")
		assert.Contains(t, state, "1") // Alice still exists
		assert.Contains(t, state, "3") // Charlie still exists
	})

	t.Run("Replay is deterministic across repeated materializations", func(t *testing.T) {
		branch, _ = branchService.GetBranchByID(branch.ID)

		reference, err := materializerService.MaterializeCollection(branch, "users")
		require.NoError(t, err)

		for i := 0; i < 20; i++ {
			state, err := materializerService.MaterializeCollection(branch, "users")
			require.NoError(t, err)
			// Content-level equality: reflect.DeepEqual through testify.
			// (Serialized bytes of Go maps are not comparable — key order
			// randomizes per marshal.)
			assert.Equal(t, reference, state, "materialization %d diverged", i)
		}
	})
}

func TestMaterializer_MaterializeBranch(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)

	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, materializerService)
	ctx := context.Background()

	t.Run("Materialize multiple collections", func(t *testing.T) {
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "u1", "name": "User1"})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "products", bson.M{"_id": "p1", "name": "Product1"})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "orders", bson.M{"_id": "o1", "user": "u1", "product": "p1"})
		assert.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)

		state, err := materializerService.MaterializeBranch(branch)
		assert.NoError(t, err)

		assert.Len(t, state, 3)
		assert.Contains(t, state, "users")
		assert.Contains(t, state, "products")
		assert.Contains(t, state, "orders")

		assert.Len(t, state["users"], 1)
		assert.Len(t, state["products"], 1)
		assert.Len(t, state["orders"], 1)
	})
}

func TestMaterializer_MaterializeDocument(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)

	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, materializerService)
	ctx := context.Background()

	t.Run("Track document history", func(t *testing.T) {
		docID := primitive.NewObjectID()

		doc := bson.M{"_id": docID, "version": 1, "status": "created"}
		_, err := interceptor.InsertOne(ctx, "docs", doc)
		assert.NoError(t, err)

		for i := 2; i <= 5; i++ {
			filter := bson.M{"_id": docID}
			update := bson.M{"$set": bson.M{"version": i, "status": "updated"}}
			_, err = interceptor.UpdateOne(ctx, "docs", filter, update, false)
			assert.NoError(t, err)
		}

		branch, _ = branchService.GetBranchByID(branch.ID)

		result, err := materializerService.MaterializeDocument(branch, "docs", docID.Hex())
		assert.NoError(t, err)
		assert.NotNil(t, result)

		assert.EqualValues(t, 5, result["version"])
		assert.Equal(t, "updated", result["status"])
	})

	t.Run("Materialize deleted document", func(t *testing.T) {
		docID := "to-delete"

		_, err := interceptor.InsertOne(ctx, "temp", bson.M{"_id": docID, "data": "test"})
		assert.NoError(t, err)

		_, err = interceptor.DeleteOne(ctx, "temp", bson.M{"_id": docID})
		assert.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)

		result, err := materializerService.MaterializeDocument(branch, "temp", docID)
		assert.NoError(t, err)
		assert.Nil(t, result) // Document was deleted
	})
}

func TestMaterializer_ComplexOperations(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)

	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, materializerService)
	ctx := context.Background()

	t.Run("Complex update operations", func(t *testing.T) {
		docID := "complex-doc"
		doc := bson.M{
			"_id": docID,
			"counters": bson.M{
				"views": int32(100),
				"likes": int32(50),
			},
			"tags": []string{"tag1", "tag2"},
			"metadata": bson.M{
				"created": "2024-01-01",
			},
		}
		_, err := interceptor.InsertOne(ctx, "analytics", doc)
		assert.NoError(t, err)

		// 1. Increment counters
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": docID},
			bson.M{"$inc": bson.M{"counters.views": int32(10), "counters.likes": int32(5)}}, false)
		assert.NoError(t, err)

		// 2. Set new field
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": docID},
			bson.M{"$set": bson.M{"metadata.updated": "2024-01-02"}}, false)
		assert.NoError(t, err)

		// 3. Unset a field
		_, err = interceptor.UpdateOne(ctx, "analytics",
			bson.M{"_id": docID},
			bson.M{"$unset": bson.M{"tags": ""}}, false)
		assert.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)

		state, err := materializerService.MaterializeCollection(branch, "analytics")
		assert.NoError(t, err)

		finalDoc := state[docID]
		assert.NotNil(t, finalDoc)

		// $inc preserves integer types instead of corrupting them to floats.
		counters := finalDoc["counters"].(bson.M)
		assert.Equal(t, int32(110), counters["views"])
		assert.Equal(t, int32(55), counters["likes"])

		metadata := finalDoc["metadata"].(bson.M)
		assert.Equal(t, "2024-01-02", metadata["updated"])
		assert.Equal(t, "2024-01-01", metadata["created"])

		assert.NotContains(t, finalDoc, "tags")
	})
}

func TestMaterializer_BranchIsolation(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)

	// Create two branches
	branch1, _ := branchService.CreateBranch("test-project", "branch-1", "")
	branch2, _ := branchService.CreateBranch("test-project", "branch-2", "")

	interceptor1 := driverwal.NewInterceptor(walService, branch1, branchService, materializerService)
	interceptor2 := driverwal.NewInterceptor(walService, branch2, branchService, materializerService)
	ctx := context.Background()

	t.Run("Branches have isolated state", func(t *testing.T) {
		_, err := interceptor1.InsertOne(ctx, "data", bson.M{"_id": "1", "branch": "one", "value": 100})
		assert.NoError(t, err)

		_, err = interceptor2.InsertOne(ctx, "data", bson.M{"_id": "1", "branch": "two", "value": 200})
		assert.NoError(t, err)

		branch1, _ = branchService.GetBranchByID(branch1.ID)
		branch2, _ = branchService.GetBranchByID(branch2.ID)

		state1, err := materializerService.MaterializeCollection(branch1, "data")
		assert.NoError(t, err)

		state2, err := materializerService.MaterializeCollection(branch2, "data")
		assert.NoError(t, err)

		assert.Equal(t, "one", state1["1"]["branch"])
		assert.EqualValues(t, 100, state1["1"]["value"])

		assert.Equal(t, "two", state2["1"]["branch"])
		assert.EqualValues(t, 200, state2["1"]["value"])
	})
}

// TestMaterializer_Ancestry covers the fork semantics: a child branch
// inherits its parent's history up to the fork point and nothing after it,
// and siblings never see each other's entries even when those entries have
// lower LSNs than the fork point.
func TestMaterializer_Ancestry(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)
	ctx := context.Background()

	main, err := branchService.CreateBranch("ancestry-test", "main", "")
	require.NoError(t, err)
	mainWriter := driverwal.NewInterceptor(walService, main, branchService, materializerService)

	// Seed main with two documents.
	_, err = mainWriter.InsertOne(ctx, "items", bson.M{"_id": "a", "v": "main-a"})
	require.NoError(t, err)
	_, err = mainWriter.InsertOne(ctx, "items", bson.M{"_id": "b", "v": "main-b"})
	require.NoError(t, err)

	t.Run("Child inherits parent state at fork", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "child", main.ID)
		require.NoError(t, err)

		state, err := materializerService.MaterializeCollection(child, "items")
		require.NoError(t, err)
		assert.Len(t, state, 2)
		assert.Equal(t, "main-a", state["a"]["v"])
	})

	t.Run("Child does not see parent writes after the fork", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "child-frozen", main.ID)
		require.NoError(t, err)

		// Parent moves on after the fork.
		_, err = mainWriter.InsertOne(ctx, "items", bson.M{"_id": "c", "v": "after-fork"})
		require.NoError(t, err)

		state, err := materializerService.MaterializeCollection(child, "items")
		require.NoError(t, err)
		assert.NotContains(t, state, "c", "entries after the fork belong to the parent only")
	})

	t.Run("Sibling writes below the fork point do not leak", func(t *testing.T) {
		// Sibling writes first, so its entries have LSNs below the fork
		// point of the branch created afterwards. The old project-wide
		// replay would have leaked them.
		main, _ = branchService.GetBranchByID(main.ID)
		sibling, err := branchService.CreateBranch("ancestry-test", "sibling", main.ID)
		require.NoError(t, err)
		siblingWriter := driverwal.NewInterceptor(walService, sibling, branchService, materializerService)
		_, err = siblingWriter.InsertOne(ctx, "items", bson.M{"_id": "s", "v": "sibling-only"})
		require.NoError(t, err)

		main, _ = branchService.GetBranchByID(main.ID)
		late, err := branchService.CreateBranch("ancestry-test", "late", main.ID)
		require.NoError(t, err)

		state, err := materializerService.MaterializeCollection(late, "items")
		require.NoError(t, err)
		assert.NotContains(t, state, "s", "sibling entries must not contaminate other branches")
	})

	t.Run("Grandchild chains through two hops", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "gen2", main.ID)
		require.NoError(t, err)
		childWriter := driverwal.NewInterceptor(walService, child, branchService, materializerService)
		_, err = childWriter.InsertOne(ctx, "items", bson.M{"_id": "g2", "v": "child-write"})
		require.NoError(t, err)

		child, _ = branchService.GetBranchByID(child.ID)
		grandchild, err := branchService.CreateBranch("ancestry-test", "gen3", child.ID)
		require.NoError(t, err)

		state, err := materializerService.MaterializeCollection(grandchild, "items")
		require.NoError(t, err)
		assert.Contains(t, state, "a", "root entries inherited")
		assert.Contains(t, state, "g2", "parent entries inherited")
	})
}

func TestMaterializer_FilterOperators(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	materializerService := materializer.NewService(walService, branchService)

	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, materializerService)
	collection := driverwal.NewCollection("test_collection", branch, walService, branchService, materializerService)
	ctx := context.Background()

	// Insert test data
	docs := []bson.M{
		{"_id": "1", "value": 10, "category": "A"},
		{"_id": "2", "value": 20, "category": "B"},
		{"_id": "3", "value": 30, "category": "A"},
		{"_id": "4", "value": 40, "category": "C"},
		{"_id": "5", "value": 50, "category": "B"},
	}

	for _, doc := range docs {
		_, err := interceptor.InsertOne(ctx, "test_collection", doc)
		assert.NoError(t, err)
	}

	// Update branch to get latest HEAD
	branch, _ = branchService.GetBranchByID(branch.ID)
	// Update collection with new branch
	collection = driverwal.NewCollection("test_collection", branch, walService, branchService, materializerService)

	t.Run("Count with filter operators", func(t *testing.T) {
		testCases := []struct {
			name     string
			filter   bson.M
			expected int64
		}{
			{
				name:     "Empty filter",
				filter:   bson.M{},
				expected: 5,
			},
			{
				name:     "Exact match",
				filter:   bson.M{"category": "A"},
				expected: 2,
			},
			{
				name:     "Greater than",
				filter:   bson.M{"value": bson.M{"$gt": 25}},
				expected: 3,
			},
			{
				name:     "Less than or equal",
				filter:   bson.M{"value": bson.M{"$lte": 30}},
				expected: 3,
			},
			{
				name:     "In array",
				filter:   bson.M{"category": bson.M{"$in": []interface{}{"A", "C"}}},
				expected: 3,
			},
			{
				name:     "Not equal",
				filter:   bson.M{"category": bson.M{"$ne": "B"}},
				expected: 3,
			},
			{
				name:     "Logical or",
				filter:   bson.M{"$or": []interface{}{bson.M{"category": "A"}, bson.M{"value": bson.M{"$gte": 50}}}},
				expected: 3,
			},
			{
				name:     "Exists",
				filter:   bson.M{"missing_field": bson.M{"$exists": false}},
				expected: 5,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				count, err := collection.CountDocuments(ctx, tc.filter)
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, count, "Filter: %v", tc.filter)
			})
		}
	})

	t.Run("Find returns a usable cursor", func(t *testing.T) {
		cursor, err := collection.Find(ctx, bson.M{"category": "A"})
		require.NoError(t, err)
		require.NotNil(t, cursor, "Find must return a real cursor")

		var results []bson.M
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})

	t.Run("Unsupported operator fails loudly", func(t *testing.T) {
		_, err := collection.CountDocuments(ctx, bson.M{"value": bson.M{"$mod": []interface{}{2, 0}}})
		assert.Error(t, err, "unknown operators must not be silently ignored")
	})
}
