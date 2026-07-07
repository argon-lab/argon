package wal_test

import (
	"context"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// newMaterializerFixture wires the WAL, branch service and materializer
// over a fresh branch with a writer.
func newMaterializerFixture(t *testing.T, db *mongo.Database, project, branchName string) (*wal.Service, *branchwal.BranchService, *materializer.Service, *wal.Branch, *walwriter.Writer) {
	t.Helper()
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	branch, err := branchService.CreateBranch(project, branchName, "")
	require.NoError(t, err)
	writer := walwriter.New(walService, branchService, mat, branch)
	return walService, branchService, mat, branch, writer
}

func TestMaterializer_MaterializeCollection(t *testing.T) {
	db := setupTestDB(t)
	_, branchService, mat, branch, writer := newMaterializerFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	t.Run("Materialize empty collection", func(t *testing.T) {
		state, err := mat.MaterializeCollection(branch, "empty_collection")
		assert.NoError(t, err)
		assert.Empty(t, state)
	})

	t.Run("Materialize collection with puts", func(t *testing.T) {
		docs := []bson.M{
			{"_id": "1", "name": "Alice", "age": int32(30)},
			{"_id": "2", "name": "Bob", "age": int32(25)},
			{"_id": "3", "name": "Charlie", "age": int32(35)},
		}
		_, err := writer.PutMany(ctx, "users", docs)
		require.NoError(t, err)

		branch, _ = branchService.GetBranchByID(branch.ID)
		state, err := mat.MaterializeCollection(branch, "users")
		assert.NoError(t, err)
		assert.Len(t, state, 3)
		assert.Equal(t, "Alice", state["1"]["name"])
		assert.Equal(t, "Bob", state["2"]["name"])
	})

	t.Run("Later puts overwrite, deletes remove", func(t *testing.T) {
		_, err := writer.Put(ctx, "users", bson.M{"_id": "1", "name": "Alice", "age": int32(31), "city": "New York"})
		require.NoError(t, err)
		_, existed, err := writer.Delete(ctx, "users", "2")
		require.NoError(t, err)
		assert.True(t, existed)

		branch, _ = branchService.GetBranchByID(branch.ID)
		state, err := mat.MaterializeCollection(branch, "users")
		assert.NoError(t, err)
		assert.Len(t, state, 2)
		assert.EqualValues(t, 31, state["1"]["age"])
		assert.Equal(t, "New York", state["1"]["city"])
		assert.NotContains(t, state, "2")
	})

	t.Run("Deleting a missing document is a no-op", func(t *testing.T) {
		before, _ := branchService.GetBranchByID(branch.ID)
		_, existed, err := writer.Delete(ctx, "users", "nonexistent")
		require.NoError(t, err)
		assert.False(t, existed)
		after, _ := branchService.GetBranchByID(branch.ID)
		assert.Equal(t, before.HeadLSN, after.HeadLSN, "no entry appended for a no-op delete")
	})

	t.Run("Replay is deterministic across repeated materializations", func(t *testing.T) {
		branch, _ = branchService.GetBranchByID(branch.ID)
		reference, err := mat.MaterializeCollection(branch, "users")
		require.NoError(t, err)
		for i := 0; i < 20; i++ {
			state, err := mat.MaterializeCollection(branch, "users")
			require.NoError(t, err)
			assert.Equal(t, reference, state, "materialization %d diverged", i)
		}
	})
}

func TestMaterializer_PutRecordsPreImages(t *testing.T) {
	db := setupTestDB(t)
	walService, branchService, _, branch, writer := newMaterializerFixture(t, db, "pre-project", "main")
	ctx := context.Background()

	_, err := writer.Put(ctx, "docs", bson.M{"_id": "d", "v": int32(1)})
	require.NoError(t, err)
	lsn, err := writer.Put(ctx, "docs", bson.M{"_id": "d", "v": int32(2)})
	require.NoError(t, err)

	entry, err := walService.GetEntry(branch.ProjectID, lsn)
	require.NoError(t, err)
	require.NotEmpty(t, entry.PreImage, "overwriting put must carry the replaced document")
	var pre bson.M
	require.NoError(t, bson.Unmarshal(entry.PreImage, &pre))
	assert.EqualValues(t, 1, pre["v"])

	_, existed, err := writer.Delete(ctx, "docs", "d")
	require.NoError(t, err)
	require.True(t, existed)
	branch, _ = branchService.GetBranchByID(branch.ID)
	entries, err := walService.GetBranchEntries(branch.ID, "docs", 0, branch.HeadLSN)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.NoError(t, bson.Unmarshal(last.PreImage, &pre))
	assert.EqualValues(t, 2, pre["v"], "delete pre-image captures the removed document")
}

func TestMaterializer_MaterializeBranch(t *testing.T) {
	db := setupTestDB(t)
	_, branchService, mat, branch, writer := newMaterializerFixture(t, db, "multi-coll", "main")
	ctx := context.Background()

	_, err := writer.Put(ctx, "users", bson.M{"_id": "u1", "name": "User1"})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "products", bson.M{"_id": "p1", "name": "Product1"})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "orders", bson.M{"_id": "o1", "user": "u1", "product": "p1"})
	require.NoError(t, err)

	branch, _ = branchService.GetBranchByID(branch.ID)
	state, err := mat.MaterializeBranch(branch)
	assert.NoError(t, err)
	assert.Len(t, state, 3)
	assert.Len(t, state["users"], 1)
	assert.Len(t, state["products"], 1)
	assert.Len(t, state["orders"], 1)
}

func TestMaterializer_MaterializeDocument(t *testing.T) {
	db := setupTestDB(t)
	_, branchService, mat, branch, writer := newMaterializerFixture(t, db, "doc-project", "main")
	ctx := context.Background()

	for v := 1; v <= 5; v++ {
		_, err := writer.Put(ctx, "docs", bson.M{"_id": "tracked", "version": int32(v)})
		require.NoError(t, err)
	}
	branch, _ = branchService.GetBranchByID(branch.ID)

	doc, err := mat.MaterializeDocument(branch, "docs", "tracked")
	require.NoError(t, err)
	require.NotNil(t, doc)
	assert.EqualValues(t, 5, doc["version"])

	_, _, err = writer.Delete(ctx, "docs", "tracked")
	require.NoError(t, err)
	branch, _ = branchService.GetBranchByID(branch.ID)
	doc, err = mat.MaterializeDocument(branch, "docs", "tracked")
	require.NoError(t, err)
	assert.Nil(t, doc, "deleted document materializes as nil")
}

func TestMaterializer_BranchIsolation(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	ctx := context.Background()

	branch1, _ := branchService.CreateBranch("iso-project", "branch-1", "")
	branch2, _ := branchService.CreateBranch("iso-project", "branch-2", "")
	writer1 := walwriter.New(walService, branchService, mat, branch1)
	writer2 := walwriter.New(walService, branchService, mat, branch2)

	_, err = writer1.Put(ctx, "data", bson.M{"_id": "1", "branch": "one", "value": int32(100)})
	require.NoError(t, err)
	_, err = writer2.Put(ctx, "data", bson.M{"_id": "1", "branch": "two", "value": int32(200)})
	require.NoError(t, err)

	branch1, _ = branchService.GetBranchByID(branch1.ID)
	branch2, _ = branchService.GetBranchByID(branch2.ID)

	state1, err := mat.MaterializeCollection(branch1, "data")
	require.NoError(t, err)
	state2, err := mat.MaterializeCollection(branch2, "data")
	require.NoError(t, err)

	assert.Equal(t, "one", state1["1"]["branch"])
	assert.Equal(t, "two", state2["1"]["branch"])
}

// TestMaterializer_Ancestry covers the fork semantics: a child branch
// inherits its parent's history up to the fork point and nothing after it,
// and siblings never see each other's entries even when those entries have
// lower LSNs than the fork point.
func TestMaterializer_Ancestry(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	mat := materializer.NewService(walService, branchService)
	ctx := context.Background()

	main, err := branchService.CreateBranch("ancestry-test", "main", "")
	require.NoError(t, err)
	mainWriter := walwriter.New(walService, branchService, mat, main)

	_, err = mainWriter.Put(ctx, "items", bson.M{"_id": "a", "v": "main-a"})
	require.NoError(t, err)
	_, err = mainWriter.Put(ctx, "items", bson.M{"_id": "b", "v": "main-b"})
	require.NoError(t, err)

	t.Run("Child inherits parent state at fork", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "child", main.ID)
		require.NoError(t, err)

		state, err := mat.MaterializeCollection(child, "items")
		require.NoError(t, err)
		assert.Len(t, state, 2)
		assert.Equal(t, "main-a", state["a"]["v"])
	})

	t.Run("Child does not see parent writes after the fork", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "child-frozen", main.ID)
		require.NoError(t, err)

		_, err = mainWriter.Put(ctx, "items", bson.M{"_id": "c", "v": "after-fork"})
		require.NoError(t, err)

		state, err := mat.MaterializeCollection(child, "items")
		require.NoError(t, err)
		assert.NotContains(t, state, "c", "entries after the fork belong to the parent only")
	})

	t.Run("Sibling writes below the fork point do not leak", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		sibling, err := branchService.CreateBranch("ancestry-test", "sibling", main.ID)
		require.NoError(t, err)
		siblingWriter := walwriter.New(walService, branchService, mat, sibling)
		_, err = siblingWriter.Put(ctx, "items", bson.M{"_id": "s", "v": "sibling-only"})
		require.NoError(t, err)

		main, _ = branchService.GetBranchByID(main.ID)
		late, err := branchService.CreateBranch("ancestry-test", "late", main.ID)
		require.NoError(t, err)

		state, err := mat.MaterializeCollection(late, "items")
		require.NoError(t, err)
		assert.NotContains(t, state, "s", "sibling entries must not contaminate other branches")
	})

	t.Run("Grandchild chains through two hops", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		child, err := branchService.CreateBranch("ancestry-test", "gen2", main.ID)
		require.NoError(t, err)
		childWriter := walwriter.New(walService, branchService, mat, child)
		_, err = childWriter.Put(ctx, "items", bson.M{"_id": "g2", "v": "child-write"})
		require.NoError(t, err)

		child, _ = branchService.GetBranchByID(child.ID)
		grandchild, err := branchService.CreateBranch("ancestry-test", "gen3", child.ID)
		require.NoError(t, err)

		state, err := mat.MaterializeCollection(grandchild, "items")
		require.NoError(t, err)
		assert.Contains(t, state, "a", "root entries inherited")
		assert.Contains(t, state, "g2", "parent entries inherited")
	})
}
