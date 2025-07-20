package importer

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/wal"
	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
)

// ImportService handles importing existing MongoDB databases into Argon WAL system
type ImportService struct {
	walService      *wal.Service
	projectService  *projectwal.ProjectService
	branchService   *branchwal.BranchService
}

// ImportPreview contains information about what would be imported
type ImportPreview struct {
	DatabaseName    string            `json:"database_name"`
	Collections     []CollectionInfo  `json:"collections"`
	TotalDocuments  int64             `json:"total_documents"`
	EstimatedSize   int64             `json:"estimated_size_bytes"`
	EstimatedWALEntries int64         `json:"estimated_wal_entries"`
}

// CollectionInfo contains details about a collection to be imported
type CollectionInfo struct {
	Name          string `json:"name"`
	DocumentCount int64  `json:"document_count"`
	SizeBytes     int64  `json:"size_bytes"`
	IndexCount    int    `json:"index_count"`
}

// ImportOptions configures the import process
type ImportOptions struct {
	MongoURI     string `json:"mongo_uri"`
	DatabaseName string `json:"database_name"`
	ProjectName  string `json:"project_name"`
	DryRun       bool   `json:"dry_run"`
	BatchSize    int    `json:"batch_size"`
}

// ImportResult contains the result of an import operation
type ImportResult struct {
	ProjectID       string            `json:"project_id"`
	BranchID        string            `json:"branch_id"`
	ImportedDocs    int64             `json:"imported_documents"`
	WALEntries      int64             `json:"wal_entries_created"`
	Collections     []string          `json:"imported_collections"`
	Duration        time.Duration     `json:"duration"`
	StartLSN        int64             `json:"start_lsn"`
	EndLSN          int64             `json:"end_lsn"`
}

// NewImportService creates a new import service
func NewImportService(walService *wal.Service, projectService *projectwal.ProjectService, branchService *branchwal.BranchService) *ImportService {
	return &ImportService{
		walService:     walService,
		projectService: projectService,
		branchService:  branchService,
	}
}

// PreviewImport analyzes a MongoDB database and returns import preview information
func (s *ImportService) PreviewImport(ctx context.Context, mongoURI, databaseName string) (*ImportPreview, error) {
	// Connect to source MongoDB
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to source MongoDB: %w", err)
	}
	defer client.Disconnect(ctx)

	// Check connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping source MongoDB: %w", err)
	}

	db := client.Database(databaseName)
	
	// List all collections
	collectionNames, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}

	preview := &ImportPreview{
		DatabaseName: databaseName,
		Collections:  make([]CollectionInfo, 0, len(collectionNames)),
	}

	// Analyze each collection
	for _, collName := range collectionNames {
		// Skip system collections
		if isSystemCollection(collName) {
			continue
		}

		collection := db.Collection(collName)
		
		// Get document count
		docCount, err := collection.EstimatedDocumentCount(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to count documents in collection %s: %w", collName, err)
		}

		// Get collection stats for size estimation
		var stats bson.M
		err = db.RunCommand(ctx, bson.D{
			{Key: "collStats", Value: collName},
		}).Decode(&stats)
		
		sizeBytes := int64(0)
		if err == nil {
			if size, ok := stats["size"].(int32); ok {
				sizeBytes = int64(size)
			} else if size, ok := stats["size"].(int64); ok {
				sizeBytes = size
			}
		}

		// Get index count
		indexes, err := collection.Indexes().List(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list indexes for collection %s: %w", collName, err)
		}

		indexCount := 0
		for indexes.Next(ctx) {
			indexCount++
		}

		collInfo := CollectionInfo{
			Name:          collName,
			DocumentCount: docCount,
			SizeBytes:     sizeBytes,
			IndexCount:    indexCount,
		}

		preview.Collections = append(preview.Collections, collInfo)
		preview.TotalDocuments += docCount
		preview.EstimatedSize += sizeBytes
	}

	// Estimate WAL entries (one per document + collection creation entries)
	preview.EstimatedWALEntries = preview.TotalDocuments + int64(len(preview.Collections))

	return preview, nil
}

// ImportDatabase imports an existing MongoDB database into Argon WAL system
func (s *ImportService) ImportDatabase(ctx context.Context, opts ImportOptions) (*ImportResult, error) {
	startTime := time.Now()

	// Validate options
	if err := s.validateImportOptions(opts); err != nil {
		return nil, fmt.Errorf("invalid import options: %w", err)
	}

	// Set default batch size if not specified
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}

	// Connect to source MongoDB
	sourceClient, err := mongo.Connect(ctx, options.Client().ApplyURI(opts.MongoURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to source MongoDB: %w", err)
	}
	defer sourceClient.Disconnect(ctx)

	if err := sourceClient.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping source MongoDB: %w", err)
	}

	sourceDB := sourceClient.Database(opts.DatabaseName)

	// Check if project already exists
	existingProject, err := s.projectService.GetProjectByName(opts.ProjectName)
	if err == nil && existingProject != nil {
		return nil, fmt.Errorf("project '%s' already exists", opts.ProjectName)
	}

	// Create new project if not in dry run mode
	var project *wal.Project
	var branch *wal.Branch
	if !opts.DryRun {
		project, err = s.projectService.CreateProject(opts.ProjectName)
		if err != nil {
			return nil, fmt.Errorf("failed to create project: %w", err)
		}

		// Get the default main branch
		branch, err = s.branchService.GetBranch(project.ID, "main")
		if err != nil {
			return nil, fmt.Errorf("failed to get main branch: %w", err)
		}
	}

	// Get list of collections to import
	collectionNames, err := sourceDB.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}

	result := &ImportResult{
		Collections: make([]string, 0),
		Duration:    0,
	}

	if !opts.DryRun {
		result.ProjectID = project.ID
		result.BranchID = branch.ID
		result.StartLSN = s.walService.GetCurrentLSN()
	}

	// Import each collection
	for _, collName := range collectionNames {
		// Skip system collections
		if isSystemCollection(collName) {
			continue
		}

		if opts.DryRun {
			// In dry run, just count documents
			collection := sourceDB.Collection(collName)
			count, err := collection.EstimatedDocumentCount(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to count documents in collection %s: %w", collName, err)
			}
			result.ImportedDocs += count
			result.Collections = append(result.Collections, collName)
		} else {
			// Actually import the collection
			imported, walEntries, err := s.importCollection(ctx, sourceDB, collName, branch, opts.BatchSize)
			if err != nil {
				return nil, fmt.Errorf("failed to import collection %s: %w", collName, err)
			}
			result.ImportedDocs += imported
			result.WALEntries += walEntries
			result.Collections = append(result.Collections, collName)
		}
	}

	if !opts.DryRun {
		result.EndLSN = s.walService.GetCurrentLSN()
	}

	result.Duration = time.Since(startTime)
	return result, nil
}

// importCollection imports a single collection into the WAL system
func (s *ImportService) importCollection(ctx context.Context, sourceDB *mongo.Database, collectionName string, branch *wal.Branch, batchSize int) (int64, int64, error) {
	collection := sourceDB.Collection(collectionName)
	
	// Create WAL interceptor for this branch
	interceptor := driverwal.NewInterceptor(s.walService, branch, s.branchService)
	
	// Create a cursor to read all documents
	cursor, err := collection.Find(ctx, bson.D{})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to create cursor for collection %s: %w", collectionName, err)
	}
	defer cursor.Close(ctx)

	var importedCount int64
	var walEntriesCount int64
	documents := make([]interface{}, 0, batchSize)

	startWALCount := s.walService.GetCurrentLSN()

	// Process documents in batches
	for cursor.Next(ctx) {
		var doc bson.M
		if err := cursor.Decode(&doc); err != nil {
			return importedCount, walEntriesCount, fmt.Errorf("failed to decode document: %w", err)
		}

		documents = append(documents, doc)

		// Process batch when it's full
		if len(documents) >= batchSize {
			if err := s.processBatch(ctx, interceptor, collectionName, documents); err != nil {
				return importedCount, walEntriesCount, fmt.Errorf("failed to process batch: %w", err)
			}
			importedCount += int64(len(documents))
			documents = documents[:0] // Reset batch
		}
	}

	// Process remaining documents
	if len(documents) > 0 {
		if err := s.processBatch(ctx, interceptor, collectionName, documents); err != nil {
			return importedCount, walEntriesCount, fmt.Errorf("failed to process final batch: %w", err)
		}
		importedCount += int64(len(documents))
	}

	if err := cursor.Err(); err != nil {
		return importedCount, walEntriesCount, fmt.Errorf("cursor error: %w", err)
	}

	endWALCount := s.walService.GetCurrentLSN()
	walEntriesCount = endWALCount - startWALCount

	return importedCount, walEntriesCount, nil
}

// processBatch processes a batch of documents through the WAL interceptor
func (s *ImportService) processBatch(ctx context.Context, interceptor *driverwal.Interceptor, collectionName string, documents []interface{}) error {
	// Use the interceptor to insert documents, which will create WAL entries
	for _, doc := range documents {
		_, err := interceptor.InsertOne(ctx, collectionName, doc)
		if err != nil {
			return fmt.Errorf("failed to insert document via interceptor: %w", err)
		}
	}
	return nil
}

// validateImportOptions validates the provided import options
func (s *ImportService) validateImportOptions(opts ImportOptions) error {
	if opts.MongoURI == "" {
		return fmt.Errorf("mongo_uri is required")
	}
	if opts.DatabaseName == "" {
		return fmt.Errorf("database_name is required")
	}
	if opts.ProjectName == "" {
		return fmt.Errorf("project_name is required")
	}
	return nil
}

// isSystemCollection returns true if the collection name represents a system collection
func isSystemCollection(name string) bool {
	systemPrefixes := []string{
		"system.",
		"admin.",
		"local.",
		"config.",
		"argon_wal.", // Don't import our own WAL collections
	}
	
	for _, prefix := range systemPrefixes {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}