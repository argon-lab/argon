package wal_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/argon-lab/argon/internal/importer"
	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wal"
)

// TestImportPreview tests the import preview functionality
func TestImportPreview(t *testing.T) {
	ctx := context.Background()
	
	// Setup test databases
	walDB := setupTestDB(t)
	sourceDB := setupTestSourceDB(t, "test_source_import")

	// Create test data in source database
	createTestImportData(t, sourceDB)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test preview
	sourceMongoURI := getTestMongoURI()
	preview, err := importService.PreviewImport(ctx, sourceMongoURI, "test_source_import")
	require.NoError(t, err)

	// Verify preview results
	assert.Equal(t, "test_source_import", preview.DatabaseName)
	assert.Equal(t, int64(3), preview.TotalDocuments)
	assert.Len(t, preview.Collections, 2) // users and products collections
	assert.Greater(t, preview.EstimatedSize, int64(0))
	assert.Equal(t, int64(5), preview.EstimatedWALEntries) // 3 docs + 2 collection creations

	// Verify collections
	collectionNames := make([]string, len(preview.Collections))
	for i, coll := range preview.Collections {
		collectionNames[i] = coll.Name
	}
	assert.Contains(t, collectionNames, "users")
	assert.Contains(t, collectionNames, "products")

	// Verify document counts
	for _, coll := range preview.Collections {
		switch coll.Name {
		case "users":
			assert.Equal(t, int64(2), coll.DocumentCount)
		case "products":
			assert.Equal(t, int64(1), coll.DocumentCount)
		}
	}
}

// TestImportDatabaseDryRun tests the dry run import functionality
func TestImportDatabaseDryRun(t *testing.T) {
	ctx := context.Background()
	
	// Setup test databases
	walDB := setupTestDB(t)
	sourceDB := setupTestSourceDB(t, "test_source_import_dry")

	// Create test data in source database
	createTestImportData(t, sourceDB)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test dry run import
	sourceMongoURI := getTestMongoURI()
	opts := importer.ImportOptions{
		MongoURI:     sourceMongoURI,
		DatabaseName: "test_source_import_dry",
		ProjectName:  "test-import-project",
		DryRun:       true,
		BatchSize:    100,
	}

	result, err := importService.ImportDatabase(ctx, opts)
	require.NoError(t, err)

	// Verify dry run results
	assert.Equal(t, int64(3), result.ImportedDocs)
	assert.Equal(t, int64(0), result.WALEntries) // No WAL entries in dry run
	assert.Len(t, result.Collections, 2)
	assert.Contains(t, result.Collections, "users")
	assert.Contains(t, result.Collections, "products")
	assert.Greater(t, result.Duration, time.Duration(0))

	// Verify no project was created
	_, err = projectService.GetProjectByName("test-import-project")
	assert.Error(t, err) // Should not exist
}

// TestImportDatabaseActual tests the actual import functionality
func TestImportDatabaseActual(t *testing.T) {
	ctx := context.Background()
	
	// Setup test databases
	walDB := setupTestDB(t)
	sourceDB := setupTestSourceDB(t, "test_source_import_actual")

	// Create test data in source database
	createTestImportData(t, sourceDB)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test actual import
	sourceMongoURI := getTestMongoURI()
	opts := importer.ImportOptions{
		MongoURI:     sourceMongoURI,
		DatabaseName: "test_source_import_actual",
		ProjectName:  "test-import-project",
		DryRun:       false,
		BatchSize:    2, // Small batch for testing
	}

	result, err := importService.ImportDatabase(ctx, opts)
	require.NoError(t, err)

	// Verify import results
	assert.Equal(t, int64(3), result.ImportedDocs)
	assert.Greater(t, result.WALEntries, int64(0)) // Should have WAL entries
	assert.Len(t, result.Collections, 2)
	assert.Contains(t, result.Collections, "users")
	assert.Contains(t, result.Collections, "products")
	assert.Greater(t, result.Duration, time.Duration(0))
	assert.NotEmpty(t, result.ProjectID)
	assert.NotEmpty(t, result.BranchID)
	assert.Greater(t, result.EndLSN, result.StartLSN)

	// Verify project was created
	project, err := projectService.GetProjectByName("test-import-project")
	require.NoError(t, err)
	assert.Equal(t, "test-import-project", project.Name)
	assert.Equal(t, project.ID, result.ProjectID)

	// Verify branch exists
	branch, err := branchService.GetBranch(project.ID, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", branch.Name)
	assert.Equal(t, branch.ID, result.BranchID)

	// Verify WAL entries were created
	finalLSN := walService.GetCurrentLSN()
	assert.Greater(t, finalLSN, result.StartLSN)

	// Verify we can query the imported data via WAL
	entries, err := walService.GetBranchEntries(branch.ID, "", result.StartLSN, result.EndLSN)
	require.NoError(t, err)
	assert.Greater(t, len(entries), 0) // Should have some entries

	// Verify entries contain our imported data
	userEntries := 0
	productEntries := 0
	for _, entry := range entries {
		switch entry.Collection {
		case "users":
			userEntries++
		case "products":
			productEntries++
		}
	}
	assert.Equal(t, 2, userEntries)
	assert.Equal(t, 1, productEntries)
}

// TestImportValidation tests input validation
func TestImportValidation(t *testing.T) {
	ctx := context.Background()
	
	// Setup test database
	walDB := setupTestDB(t)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test missing URI
	opts := importer.ImportOptions{
		DatabaseName: "test",
		ProjectName:  "test-project",
	}
	_, err = importService.ImportDatabase(ctx, opts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mongo_uri is required")

	// Test missing database name
	opts = importer.ImportOptions{
		MongoURI:    "mongodb://localhost:27017",
		ProjectName: "test-project",
	}
	_, err = importService.ImportDatabase(ctx, opts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database_name is required")

	// Test missing project name
	opts = importer.ImportOptions{
		MongoURI:     "mongodb://localhost:27017",
		DatabaseName: "test",
	}
	_, err = importService.ImportDatabase(ctx, opts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "project_name is required")
}

// TestImportDuplicateProject tests handling of duplicate project names
func TestImportDuplicateProject(t *testing.T) {
	ctx := context.Background()
	
	// Setup test database
	walDB := setupTestDB(t)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Create a project first
	_, err = projectService.CreateProject("existing-project")
	require.NoError(t, err)

	// Try to import with same project name
	opts := importer.ImportOptions{
		MongoURI:     getTestMongoURI(),
		DatabaseName: "test_db",
		ProjectName:  "existing-project",
		DryRun:       false,
	}

	_, err = importService.ImportDatabase(ctx, opts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "project 'existing-project' already exists")
}

// TestImportInvalidDatabase tests handling of invalid database connections
func TestImportInvalidDatabase(t *testing.T) {
	ctx := context.Background()
	
	// Setup test database
	walDB := setupTestDB(t)

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test invalid MongoDB URI
	_, err = importService.PreviewImport(ctx, "invalid://uri", "test_db")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to source MongoDB")

	// Test non-existent database (this should work but return empty preview)
	validURI := getTestMongoURI()
	preview, err := importService.PreviewImport(ctx, validURI, "nonexistent_database")
	require.NoError(t, err)
	assert.Equal(t, "nonexistent_database", preview.DatabaseName)
	assert.Equal(t, int64(0), preview.TotalDocuments)
	assert.Len(t, preview.Collections, 0)
}

// TestImportSystemCollections tests that system collections are properly filtered
func TestImportSystemCollections(t *testing.T) {
	ctx := context.Background()
	
	// Setup test databases
	walDB := setupTestDB(t)
	sourceDB := setupTestSourceDB(t, "test_source_import_system")

	// Create test data including system-like collections
	sourceDB.Collection("users").InsertOne(ctx, bson.M{"name": "user1"})
	sourceDB.Collection("system.indexes").InsertOne(ctx, bson.M{"index": "test"})
	sourceDB.Collection("admin.users").InsertOne(ctx, bson.M{"admin": "test"})
	sourceDB.Collection("local.test").InsertOne(ctx, bson.M{"local": "test"})
	sourceDB.Collection("argon_wal.test").InsertOne(ctx, bson.M{"wal": "test"})

	// Setup services
	walService, err := wal.NewService(walDB)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(walDB, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(walDB, walService, branchService)
	require.NoError(t, err)

	importService := importer.NewImportService(walService, projectService, branchService)

	// Test preview
	sourceMongoURI := getTestMongoURI()
	preview, err := importService.PreviewImport(ctx, sourceMongoURI, "test_source_import_system")
	require.NoError(t, err)

	// Verify only non-system collections are included
	assert.Equal(t, int64(1), preview.TotalDocuments) // Only users collection
	assert.Len(t, preview.Collections, 1)
	assert.Equal(t, "users", preview.Collections[0].Name)
}

// createTestImportData creates test data for import testing
func createTestImportData(t *testing.T, db *mongo.Database) {
	ctx := context.Background()

	// Create users collection with test data
	usersCollection := db.Collection("users")
	users := []interface{}{
		bson.M{"_id": "user1", "name": "John Doe", "email": "john@example.com", "age": 30},
		bson.M{"_id": "user2", "name": "Jane Smith", "email": "jane@example.com", "age": 25},
	}
	_, err := usersCollection.InsertMany(ctx, users)
	require.NoError(t, err)

	// Create products collection with test data
	productsCollection := db.Collection("products")
	products := []interface{}{
		bson.M{"_id": "prod1", "name": "Widget", "price": 29.99, "category": "tools"},
	}
	_, err = productsCollection.InsertMany(ctx, products)
	require.NoError(t, err)
}


// setupTestSourceDB creates a separate source database for import testing
func setupTestSourceDB(t *testing.T, dbName string) *mongo.Database {
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)

	// Use the provided database name
	db := client.Database(dbName)

	// Clean up any existing data
	err = db.Drop(ctx)
	if err != nil {
		t.Logf("Warning: could not drop source database %s: %v", dbName, err)
	}

	// Clean up after test
	t.Cleanup(func() {
		err := db.Drop(context.Background())
		if err != nil {
			t.Logf("Failed to drop source database: %v", err)
		}
		err = client.Disconnect(context.Background())
		if err != nil {
			t.Logf("Failed to disconnect: %v", err)
		}
	})

	return db
}

// getTestMongoURI returns the MongoDB URI for testing
func getTestMongoURI() string {
	// Use environment variable if set, otherwise default to localhost
	if uri := os.Getenv("TEST_MONGODB_URI"); uri != "" {
		return uri
	}
	return "mongodb://localhost:27017"
}