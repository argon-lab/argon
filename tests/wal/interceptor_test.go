package wal_test

import (
	"context"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestInterceptor_InsertOne(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	// Create a test branch
	branch, err := branchService.CreateBranch("test-project", "test-branch", "")
	require.NoError(t, err)
	
	// Create interceptor
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	t.Run("Insert with generated ID", func(t *testing.T) {
		doc := bson.M{
			"name": "Alice",
			"age":  30,
		}
		
		result, err := interceptor.InsertOne(context.Background(), "users", doc)
		assert.NoError(t, err)
		assert.NotNil(t, result.InsertedID)
		
		// Verify WAL entry was created
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, wal.OpInsert, entries[0].Operation)
		
		// Verify branch HEAD was updated
		updatedBranch, err := branchService.GetBranchByID(branch.ID)
		assert.NoError(t, err)
		assert.Greater(t, updatedBranch.HeadLSN, branch.HeadLSN)
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
		
		// Verify document ID in WAL entry
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		
		// Find the entry for Bob
		var bobEntry *wal.Entry
		for _, entry := range entries {
			if entry.DocumentID == docID.Hex() {
				bobEntry = entry
				break
			}
		}
		
		assert.NotNil(t, bobEntry)
		
		// Verify document content
		var savedDoc bson.M
		err = bson.Unmarshal(bobEntry.Document, &savedDoc)
		assert.NoError(t, err)
		assert.Equal(t, "Bob", savedDoc["name"])
	})
}

func TestInterceptor_UpdateOne(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	t.Run("Update operation", func(t *testing.T) {
		filter := bson.M{"name": "Alice"}
		update := bson.M{"$set": bson.M{"age": 31}}
		
		result, err := interceptor.UpdateOne(context.Background(), "users", filter, update)
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.MatchedCount)
		assert.Equal(t, int64(1), result.ModifiedCount)
		
		// Verify WAL entry
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		
		// Find update entry
		var updateEntry *wal.Entry
		for _, entry := range entries {
			if entry.Operation == wal.OpUpdate {
				updateEntry = entry
				break
			}
		}
		
		assert.NotNil(t, updateEntry)
		assert.Equal(t, "users", updateEntry.Collection)
		
		// Verify update document contains filter and update
		var updateDoc bson.M
		err = bson.Unmarshal(updateEntry.Document, &updateDoc)
		assert.NoError(t, err)
		assert.Contains(t, updateDoc, "filter")
		assert.Contains(t, updateDoc, "update")
	})
}

func TestInterceptor_DeleteOne(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	t.Run("Delete operation", func(t *testing.T) {
		filter := bson.M{"name": "Alice"}
		
		result, err := interceptor.DeleteOne(context.Background(), "users", filter)
		assert.NoError(t, err)
		assert.Equal(t, int64(1), result.DeletedCount)
		
		// Verify WAL entry
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		
		// Find delete entry
		var deleteEntry *wal.Entry
		for _, entry := range entries {
			if entry.Operation == wal.OpDelete {
				deleteEntry = entry
				break
			}
		}
		
		assert.NotNil(t, deleteEntry)
		assert.Equal(t, "users", deleteEntry.Collection)
		
		// Verify metadata indicates this is a filter
		assert.Equal(t, true, deleteEntry.Metadata["is_filter"])
	})
}

func TestInterceptor_InsertMany(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	branch, _ := branchService.CreateBranch("test-project", "test-branch", "")
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)
	
	t.Run("Insert multiple documents", func(t *testing.T) {
		docs := []interface{}{
			bson.M{"name": "Charlie", "age": 35},
			bson.M{"name": "David", "age": 40},
			bson.M{"name": "Eve", "age": 28},
		}
		
		insertedIDs, err := interceptor.InsertMany(context.Background(), "users", docs)
		assert.NoError(t, err)
		assert.Len(t, insertedIDs, 3)
		
		// Verify all documents have IDs
		for _, id := range insertedIDs {
			assert.NotNil(t, id)
		}
		
		// Verify WAL entries
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, len(entries), 3)
		
		// Count insert operations
		insertCount := 0
		for _, entry := range entries {
			if entry.Operation == wal.OpInsert {
				insertCount++
			}
		}
		assert.GreaterOrEqual(t, insertCount, 3)
	})
}

func TestInterceptor_BranchIsolation(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	
	// Create two branches
	branch1, _ := branchService.CreateBranch("test-project", "branch-1", "")
	branch2, _ := branchService.CreateBranch("test-project", "branch-2", "")
	
	interceptor1 := driverwal.NewInterceptor(walService, branch1, branchService)
	interceptor2 := driverwal.NewInterceptor(walService, branch2, branchService)
	
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
		entries1, err := walService.GetBranchEntries(branch1.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		assert.Len(t, entries1, 1)
		
		// Verify branch 2 entries
		entries2, err := walService.GetBranchEntries(branch2.ID, "users", 0, walService.GetCurrentLSN())
		assert.NoError(t, err)
		assert.Len(t, entries2, 1)
		
		// Verify different content
		var doc1Saved, doc2Saved bson.M
		bson.Unmarshal(entries1[0].Document, &doc1Saved)
		bson.Unmarshal(entries2[0].Document, &doc2Saved)
		
		assert.Equal(t, "Branch1User", doc1Saved["name"])
		assert.Equal(t, "Branch2User", doc2Saved["name"])
	})
}