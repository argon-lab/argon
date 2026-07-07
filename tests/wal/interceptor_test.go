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
	"go.mongodb.org/mongo-driver/mongo"
)

// newInterceptorFixture wires a WAL service, branch service, materializer
// and interceptor over a fresh branch.
func newInterceptorFixture(t *testing.T, db *mongo.Database, project, branchName string) (*wal.Service, *branchwal.BranchService, *wal.Branch, *driverwal.Interceptor) {
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	branch, err := branchService.CreateBranch(project, branchName, "")
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	interceptor := driverwal.NewInterceptor(walService, branch, branchService, mat)
	return walService, branchService, branch, interceptor
}

func TestInterceptor_InsertOne(t *testing.T) {
	db := setupTestDB(t)
	walService, branchService, branch, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")

	t.Run("Insert with generated ID", func(t *testing.T) {
		doc := bson.M{
			"name": "Alice",
			"age":  30,
		}

		result, err := interceptor.InsertOne(context.Background(), "users", doc)
		assert.NoError(t, err)
		assert.NotNil(t, result.InsertedID)

		// Verify WAL entry was created
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN(branch.ProjectID))
		assert.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, wal.OpPut, entries[0].Operation)
		assert.NotEmpty(t, entries[0].PostImage, "puts must carry the post-image")

		// Verify branch HEAD was updated
		updatedBranch, err := branchService.GetBranchByID(branch.ID)
		assert.NoError(t, err)
		assert.Greater(t, updatedBranch.HeadLSN, branch.BaseLSN)
	})

	t.Run("Insert with existing ID", func(t *testing.T) {
		docID := primitive.NewObjectID()
		doc := bson.M{
			"_id":  docID,
			"name": "Bob",
			"age":  25,
		}

		result, err := interceptor.InsertOne(context.Background(), "users", doc)
		assert.NoError(t, err)
		assert.Equal(t, docID, result.InsertedID)

		// Verify document content round-trips through the post-image
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN(branch.ProjectID))
		assert.NoError(t, err)

		var bobEntry *wal.Entry
		for _, entry := range entries {
			if entry.DocumentID == docID.Hex() {
				bobEntry = entry
				break
			}
		}
		require.NotNil(t, bobEntry)

		var savedDoc bson.M
		err = bson.Unmarshal(bobEntry.PostImage, &savedDoc)
		assert.NoError(t, err)
		assert.Equal(t, "Bob", savedDoc["name"])
	})

	t.Run("Duplicate insert is rejected", func(t *testing.T) {
		docID := primitive.NewObjectID()
		doc := bson.M{"_id": docID, "name": "Dup"}

		_, err := interceptor.InsertOne(context.Background(), "users", doc)
		require.NoError(t, err)

		_, err = interceptor.InsertOne(context.Background(), "users", doc)
		assert.Error(t, err, "inserting the same _id twice must fail")
		assert.Contains(t, err.Error(), "duplicate key")
	})
}

func TestInterceptor_UpdateOne(t *testing.T) {
	db := setupTestDB(t)
	walService, branchService, branch, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	t.Run("Update misses when nothing matches", func(t *testing.T) {
		result, err := interceptor.UpdateOne(ctx, "users", bson.M{"name": "Nobody"}, bson.M{"$set": bson.M{"age": 1}}, false)
		assert.NoError(t, err)
		assert.Equal(t, int64(0), result.MatchedCount, "no fabricated match counts")
		assert.Equal(t, int64(0), result.ModifiedCount)
	})

	t.Run("Update writes the post-image", func(t *testing.T) {
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "alice", "name": "Alice", "age": 30})
		require.NoError(t, err)

		result, err := interceptor.UpdateOne(ctx, "users", bson.M{"name": "Alice"}, bson.M{"$set": bson.M{"age": 31}}, false)
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.MatchedCount)
		assert.Equal(t, int64(1), result.ModifiedCount)

		// The WAL must contain a put whose post-image is the updated doc —
		// never a filter or an update expression.
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN(branch.ProjectID))
		assert.NoError(t, err)
		last := entries[len(entries)-1]
		assert.Equal(t, wal.OpPut, last.Operation)
		assert.Equal(t, "alice", last.DocumentID)

		var post bson.M
		require.NoError(t, bson.Unmarshal(last.PostImage, &post))
		assert.EqualValues(t, 31, post["age"])
		assert.Equal(t, "Alice", post["name"])

		var pre bson.M
		require.NoError(t, bson.Unmarshal(last.PreImage, &pre))
		assert.EqualValues(t, 30, pre["age"], "pre-image preserves the replaced document")
	})

	t.Run("No-op update writes nothing", func(t *testing.T) {
		before := walService.GetCurrentLSN(branch.ProjectID)
		result, err := interceptor.UpdateOne(ctx, "users", bson.M{"_id": "alice"}, bson.M{"$set": bson.M{"age": 31}}, false)
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.MatchedCount)
		assert.Equal(t, int64(0), result.ModifiedCount)
		assert.Equal(t, before, walService.GetCurrentLSN(branch.ProjectID), "matched-but-unchanged must not append")
	})

	t.Run("Upsert inserts when nothing matches", func(t *testing.T) {
		result, err := interceptor.UpdateOne(ctx, "users", bson.M{"_id": "carol"}, bson.M{"$set": bson.M{"name": "Carol"}}, true)
		assert.NoError(t, err)
		assert.Equal(t, int64(0), result.MatchedCount)
		assert.Equal(t, int64(1), result.UpsertedCount)
		assert.Equal(t, "carol", result.UpsertedID)

		doc, err := materializer.NewService(walService, branchService).MaterializeDocument(branch, "users", "carol")
		require.NoError(t, err)
		require.NotNil(t, doc)
		assert.Equal(t, "Carol", doc["name"])
	})
}

func TestInterceptor_UpdateMany(t *testing.T) {
	db := setupTestDB(t)
	_, _, _, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	docs := []interface{}{
		bson.M{"_id": "u1", "team": "red", "score": int32(1)},
		bson.M{"_id": "u2", "team": "red", "score": int32(2)},
		bson.M{"_id": "u3", "team": "blue", "score": int32(3)},
	}
	_, err := interceptor.InsertMany(ctx, "players", docs)
	require.NoError(t, err)

	result, err := interceptor.UpdateMany(ctx, "players", bson.M{"team": "red"}, bson.M{"$inc": bson.M{"score": 10}}, false)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), result.MatchedCount)
	assert.Equal(t, int64(2), result.ModifiedCount)

	matches, err := interceptor.FindMatches("players", bson.M{"score": bson.M{"$gt": 10}}, false)
	assert.NoError(t, err)
	assert.Len(t, matches, 2, "both red players should have been incremented")
}

func TestInterceptor_DeleteOne(t *testing.T) {
	db := setupTestDB(t)
	walService, _, branch, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	t.Run("Delete misses when nothing matches", func(t *testing.T) {
		result, err := interceptor.DeleteOne(ctx, "users", bson.M{"name": "Nobody"})
		assert.NoError(t, err)
		assert.Equal(t, int64(0), result.DeletedCount, "no fabricated delete counts")
	})

	t.Run("Delete records document ID and pre-image", func(t *testing.T) {
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "alice", "name": "Alice"})
		require.NoError(t, err)

		result, err := interceptor.DeleteOne(ctx, "users", bson.M{"name": "Alice"})
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.DeletedCount)

		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN(branch.ProjectID))
		assert.NoError(t, err)
		last := entries[len(entries)-1]
		assert.Equal(t, wal.OpDelete, last.Operation)
		assert.Equal(t, "alice", last.DocumentID, "deletes are logged by document ID, not filter")

		var pre bson.M
		require.NoError(t, bson.Unmarshal(last.PreImage, &pre))
		assert.Equal(t, "Alice", pre["name"], "delete pre-image preserves the removed document")
	})
}

func TestInterceptor_DeleteMany(t *testing.T) {
	db := setupTestDB(t)
	_, _, _, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	docs := []interface{}{
		bson.M{"_id": "u1", "team": "red"},
		bson.M{"_id": "u2", "team": "red"},
		bson.M{"_id": "u3", "team": "blue"},
	}
	_, err := interceptor.InsertMany(ctx, "players", docs)
	require.NoError(t, err)

	result, err := interceptor.DeleteMany(ctx, "players", bson.M{"team": "red"})
	assert.NoError(t, err)
	assert.Equal(t, int64(2), result.DeletedCount)

	remaining, err := interceptor.FindMatches("players", bson.M{}, false)
	assert.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "blue", remaining[0]["team"])
}

func TestInterceptor_ReplaceOne(t *testing.T) {
	db := setupTestDB(t)
	_, _, _, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")
	ctx := context.Background()

	_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "alice", "name": "Alice", "age": 30})
	require.NoError(t, err)

	result, err := interceptor.ReplaceOne(ctx, "users", bson.M{"_id": "alice"}, bson.M{"name": "Alicia"}, false)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), result.MatchedCount)
	assert.Equal(t, int64(1), result.ModifiedCount)

	matches, err := interceptor.FindMatches("users", bson.M{"_id": "alice"}, true)
	assert.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "Alicia", matches[0]["name"])
	_, hasAge := matches[0]["age"]
	assert.False(t, hasAge, "replacement removes fields not in the new document")
}

func TestInterceptor_InsertMany(t *testing.T) {
	db := setupTestDB(t)
	walService, _, branch, interceptor := newInterceptorFixture(t, db, "test-project", "test-branch")

	t.Run("Insert multiple documents", func(t *testing.T) {
		docs := []interface{}{
			bson.M{"name": "Charlie", "age": 35},
			bson.M{"name": "David", "age": 40},
			bson.M{"name": "Eve", "age": 28},
		}

		insertedIDs, err := interceptor.InsertMany(context.Background(), "users", docs)
		assert.NoError(t, err)
		assert.Len(t, insertedIDs, 3)

		for _, id := range insertedIDs {
			assert.NotNil(t, id)
		}

		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN(branch.ProjectID))
		assert.NoError(t, err)
		assert.Len(t, entries, 3)

		// A batch insert reserves one contiguous LSN range.
		for i := 1; i < len(entries); i++ {
			assert.Equal(t, entries[i-1].LSN+1, entries[i].LSN, "batch entries should have contiguous LSNs")
		}
	})

	t.Run("In-batch duplicate is rejected", func(t *testing.T) {
		docs := []interface{}{
			bson.M{"_id": "same", "n": 1},
			bson.M{"_id": "same", "n": 2},
		}
		_, err := interceptor.InsertMany(context.Background(), "users", docs)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate key")
	})
}

func TestInterceptor_BranchIsolation(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)

	// Create two branches
	branch1, err := branchService.CreateBranch("test-project", "branch-1", "")
	require.NoError(t, err)
	branch2, err := branchService.CreateBranch("test-project", "branch-2", "")
	require.NoError(t, err)

	interceptor1 := driverwal.NewInterceptor(walService, branch1, branchService, mat)
	interceptor2 := driverwal.NewInterceptor(walService, branch2, branchService, mat)

	t.Run("Operations isolated by branch", func(t *testing.T) {
		// Insert in branch 1
		doc1 := bson.M{"name": "Branch1User", "branch": 1}
		_, err := interceptor1.InsertOne(context.Background(), "users", doc1)
		assert.NoError(t, err)

		// Insert in branch 2
		doc2 := bson.M{"name": "Branch2User", "branch": 2}
		_, err = interceptor2.InsertOne(context.Background(), "users", doc2)
		assert.NoError(t, err)

		// Verify branch 1 entries
		entries1, err := walService.GetBranchEntries(branch1.ID, "users", 0, walService.GetCurrentLSN(branch1.ProjectID))
		assert.NoError(t, err)
		assert.Len(t, entries1, 1)

		// Verify branch 2 entries
		entries2, err := walService.GetBranchEntries(branch2.ID, "users", 0, walService.GetCurrentLSN(branch2.ProjectID))
		assert.NoError(t, err)
		assert.Len(t, entries2, 1)

		// Verify different content
		var doc1Saved, doc2Saved bson.M
		_ = bson.Unmarshal(entries1[0].PostImage, &doc1Saved)
		_ = bson.Unmarshal(entries2[0].PostImage, &doc2Saved)

		assert.Equal(t, "Branch1User", doc1Saved["name"])
		assert.Equal(t, "Branch2User", doc2Saved["name"])

		// And the materialized states don't leak into each other.
		state1, err := mat.MaterializeCollection(branch1, "users")
		require.NoError(t, err)
		state2, err := mat.MaterializeCollection(branch2, "users")
		require.NoError(t, err)
		assert.Len(t, state1, 1)
		assert.Len(t, state2, 1)
	})
}
