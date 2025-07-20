package wal_test

import (
	"context"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mustMarshalBSON marshals data to BSON or panics
func mustMarshalBSON(v interface{}) bson.Raw {
	data, err := bson.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// setupTestDB creates a test MongoDB connection
func setupTestDB(t *testing.T) *mongo.Database {
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)

	// Use a unique database name for each test
	dbName := "argon_wal_test_" + time.Now().Format("20060102150405")
	db := client.Database(dbName)

	// Clean up after test
	t.Cleanup(func() {
		err := db.Drop(context.Background())
		if err != nil {
			t.Logf("Failed to drop test database: %v", err)
		}
		err = client.Disconnect(context.Background())
		if err != nil {
			t.Logf("Failed to disconnect: %v", err)
		}
	})

	return db
}

func TestWALService_Basic(t *testing.T) {
	db := setupTestDB(t)
	
	// Create WAL service
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	// Test appending entries
	entry1 := &wal.Entry{
		ProjectID:  "test-project",
		BranchID:   "main",
		Operation:  wal.OpInsert,
		Collection: "users",
		DocumentID: "user-1",
		Document:   mustMarshalBSON(bson.M{"name": "Alice"}),
	}
	
	lsn1, err := walService.Append(entry1)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), lsn1)
	
	// Append another entry
	entry2 := &wal.Entry{
		ProjectID:  "test-project",
		BranchID:   "main",
		Operation:  wal.OpInsert,
		Collection: "users",
		DocumentID: "user-2",
		Document:   mustMarshalBSON(bson.M{"name": "Bob"}),
	}
	
	lsn2, err := walService.Append(entry2)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), lsn2)
	
	// Verify current LSN
	assert.Equal(t, int64(2), walService.GetCurrentLSN())
	
	// Retrieve entries
	entries, err := walService.GetBranchEntries("main", "users", 0, 10)
	assert.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestBranchService_CreateBranch(t *testing.T) {
	db := setupTestDB(t)
	
	// Create services
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	// Create main branch
	mainBranch, err := branchService.CreateBranch("project-1", "main", "")
	assert.NoError(t, err)
	assert.Equal(t, "main", mainBranch.Name)
	assert.Equal(t, int64(1), mainBranch.CreatedLSN)
	assert.Equal(t, int64(1), mainBranch.HeadLSN)
	assert.Equal(t, int64(0), mainBranch.BaseLSN)
	
	// Create feature branch from main
	featureBranch, err := branchService.CreateBranch("project-1", "feature-x", mainBranch.ID)
	assert.NoError(t, err)
	assert.Equal(t, "feature-x", featureBranch.Name)
	assert.Equal(t, mainBranch.HeadLSN, featureBranch.HeadLSN) // Inherits parent's HEAD
	assert.Equal(t, mainBranch.HeadLSN, featureBranch.BaseLSN) // Fork point
	
	// List branches
	branches, err := branchService.ListBranches("project-1")
	assert.NoError(t, err)
	assert.Len(t, branches, 2)
}

func TestBranchService_DeleteBranch(t *testing.T) {
	db := setupTestDB(t)
	
	// Create services
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	// Create branches
	mainBranch, err := branchService.CreateBranch("project-1", "main", "")
	require.NoError(t, err)
	
	featureBranch, err := branchService.CreateBranch("project-1", "feature-x", mainBranch.ID)
	require.NoError(t, err)
	
	// Try to delete main branch (should fail)
	err = branchService.DeleteBranch("project-1", "main")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete main branch")
	
	// Create child branch to test parent protection  
	_, err = branchService.CreateBranch("project-1", "child-branch", featureBranch.ID)
	require.NoError(t, err)
	
	// Try to delete parent branch with child (should fail)
	err = branchService.DeleteBranch("project-1", "feature-x")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete branch with active children")
	
	// Delete child branch first
	err = branchService.DeleteBranch("project-1", "child-branch")
	assert.NoError(t, err)
	
	// Now delete feature branch (should succeed)
	err = branchService.DeleteBranch("project-1", "feature-x")
	assert.NoError(t, err)
	
	// Verify branch is deleted
	_, err = branchService.GetBranch("project-1", "feature-x")
	assert.Error(t, err)
	
	// Verify WAL entries still exist
	currentLSN := walService.GetCurrentLSN()
	assert.Greater(t, currentLSN, int64(0))
}

func TestProjectService_CreateProject(t *testing.T) {
	db := setupTestDB(t)
	
	// Create services
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	
	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)
	
	// Create project
	project, err := projectService.CreateProject("ml-experiments")
	assert.NoError(t, err)
	assert.Equal(t, "ml-experiments", project.Name)
	assert.True(t, project.UseWAL)
	assert.NotEmpty(t, project.MainBranchID)
	
	// Verify main branch was created
	branches, err := branchService.ListBranches(project.ID)
	assert.NoError(t, err)
	assert.Len(t, branches, 1)
	assert.Equal(t, "main", branches[0].Name)
	
	// List projects
	projects, err := projectService.ListProjects()
	assert.NoError(t, err)
	assert.Len(t, projects, 1)
}