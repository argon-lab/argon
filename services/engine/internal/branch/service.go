package branch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"argon/engine/internal/monitoring"
	"argon/engine/internal/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Service struct {
	client     *mongo.Client
	db         *mongo.Database
	storage    storage.Service
	
	// Branch context (can be set for branch-specific operations)
	currentBranchID string
	branchDB        *BranchDatabase
}

func NewService(client *mongo.Client, storage storage.Service) *Service {
	return &Service{
		client:  client,
		db:      client.Database("argon"),
		storage: storage,
	}
}

// WithBranchContext creates a new service instance with branch context
func (s *Service) WithBranchContext(branchID string) *Service {
	branchDB := NewBranchDatabase(s.client, "argon", branchID)
	return &Service{
		client:          s.client,
		db:              s.db,
		storage:         s.storage,
		currentBranchID: branchID,
		branchDB:        branchDB,
	}
}

// GetBranchDatabase returns the branch-aware database for the current branch context
func (s *Service) GetBranchDatabase() *BranchDatabase {
	if s.branchDB == nil {
		// Default to main branch if no context set
		s.branchDB = NewBranchDatabase(s.client, "argon", "main")
	}
	return s.branchDB
}

// CreateBranch creates a new database branch with copy-on-write semantics
func (s *Service) CreateBranch(ctx context.Context, req *BranchCreateRequest) (*Branch, error) {
	start := time.Now()
	var success bool
	defer func() {
		duration := time.Since(start)
		monitoring.RecordBranchOperation(ctx, "create", duration, success)
		if success {
			monitoring.GlobalMetrics.ActiveBranches.Add(ctx, 1)
		}
	}()
	
	now := time.Now()
	
	// Generate unique storage path for this branch
	storagePath := fmt.Sprintf("projects/%s/branches/%s", req.ProjectID.Hex(), req.Name)
	
	// Create branch document
	branch := &Branch{
		ID:              primitive.NewObjectID(),
		ProjectID:       req.ProjectID,
		Name:            req.Name,
		Description:     req.Description,
		ParentBranch:    req.ParentBranch,
		Status:          BranchStatusActive,
		IsMain:          req.ParentBranch == nil, // Main branch if no parent
		BaseRevision:    generateRevision(),
		CurrentRevision: generateRevision(),
		CreatedAt:       now,
		UpdatedAt:       now,
		StoragePath:     storagePath,
		Metadata:        make(map[string]interface{}),
		DocumentCount:   0,
		StorageSize:     0,
	}
	
	// Insert branch into database
	collection := s.db.Collection("branches")
	_, err := collection.InsertOne(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("failed to create branch: %w", err)
	}
	
	// Create initial storage structure for the branch
	if err := s.initializeBranchStorage(ctx, branch); err != nil {
		// Rollback branch creation
		collection.DeleteOne(ctx, bson.M{"_id": branch.ID})
		return nil, fmt.Errorf("failed to initialize branch storage: %w", err)
	}
	
	// If this has a parent branch, copy initial data with real data isolation
	if req.ParentBranch != nil {
		if err := s.copyFromParent(ctx, branch, *req.ParentBranch); err != nil {
			// Rollback branch creation and cleanup any created collections
			collection.DeleteOne(ctx, bson.M{"_id": branch.ID})
			branchDB := NewBranchDatabase(s.client, "argon", branch.ID.Hex())
			branchDB.DeleteBranchCollections(ctx) // Clean up any partially created collections
			return nil, fmt.Errorf("failed to copy from parent branch: %w", err)
		}
	}
	
	success = true
	return branch, nil
}

// GetBranch retrieves a branch by ID
func (s *Service) GetBranch(ctx context.Context, branchID primitive.ObjectID) (*Branch, error) {
	collection := s.db.Collection("branches")
	
	var branch Branch
	err := collection.FindOne(ctx, bson.M{"_id": branchID}).Decode(&branch)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("branch not found")
		}
		return nil, fmt.Errorf("failed to get branch: %w", err)
	}
	
	return &branch, nil
}

// ListBranches lists all branches for a project
func (s *Service) ListBranches(ctx context.Context, projectID primitive.ObjectID) ([]*Branch, error) {
	collection := s.db.Collection("branches")
	
	cursor, err := collection.Find(ctx, bson.M{"project_id": projectID}, 
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}
	defer cursor.Close(ctx)
	
	var branches []*Branch
	for cursor.Next(ctx) {
		var branch Branch
		if err := cursor.Decode(&branch); err != nil {
			return nil, fmt.Errorf("failed to decode branch: %w", err)
		}
		branches = append(branches, &branch)
	}
	
	return branches, nil
}

// UpdateBranch updates a branch
func (s *Service) UpdateBranch(ctx context.Context, branchID primitive.ObjectID, req *BranchUpdateRequest) (*Branch, error) {
	collection := s.db.Collection("branches")
	
	update := bson.M{
		"updated_at": time.Now(),
	}
	
	if req.Description != nil {
		update["description"] = *req.Description
	}
	
	if req.Status != nil {
		update["status"] = *req.Status
	}
	
	_, err := collection.UpdateOne(ctx, bson.M{"_id": branchID}, bson.M{"$set": update})
	if err != nil {
		return nil, fmt.Errorf("failed to update branch: %w", err)
	}
	
	return s.GetBranch(ctx, branchID)
}

// DeleteBranch deletes a branch (can be soft or hard delete)
func (s *Service) DeleteBranch(ctx context.Context, branchID primitive.ObjectID) error {
	return s.DeleteBranchWithOptions(ctx, branchID, false)
}

// DeleteBranchWithOptions deletes a branch with options for hard delete
func (s *Service) DeleteBranchWithOptions(ctx context.Context, branchID primitive.ObjectID, hardDelete bool) error {
	// Get branch information first
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return fmt.Errorf("branch not found: %w", err)
	}
	
	// Don't allow deletion of main branch
	if branch.IsMain {
		return fmt.Errorf("cannot delete main branch")
	}
	
	collection := s.db.Collection("branches")
	
	if hardDelete {
		// Hard delete: remove collections and branch metadata
		branchDB := NewBranchDatabase(s.client, "argon", branchID.Hex())
		
		log.Printf("Hard deleting branch %s (%s) - removing all collections", branch.Name, branchID.Hex())
		
		// Delete all branch-specific collections
		if err := branchDB.DeleteBranchCollections(ctx); err != nil {
			log.Printf("Warning: failed to delete some collections for branch %s: %v", branch.Name, err)
		}
		
		// Delete branch metadata
		_, err = collection.DeleteOne(ctx, bson.M{"_id": branchID})
		if err != nil {
			return fmt.Errorf("failed to delete branch metadata: %w", err)
		}
		
		log.Printf("Successfully hard deleted branch %s", branch.Name)
	} else {
		// Soft delete: mark as archived
		update := bson.M{
			"status":     BranchStatusArchived,
			"updated_at": time.Now(),
		}
		
		_, err = collection.UpdateOne(ctx, bson.M{"_id": branchID}, bson.M{"$set": update})
		if err != nil {
			return fmt.Errorf("failed to archive branch: %w", err)
		}
		
		log.Printf("Successfully archived branch %s", branch.Name)
	}
	
	return nil
}

// SwitchBranch switches the active branch for a project
func (s *Service) SwitchBranch(ctx context.Context, projectID, branchID primitive.ObjectID) error {
	// Verify the branch exists and is active
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return fmt.Errorf("branch not found: %w", err)
	}
	
	if branch.ProjectID != projectID {
		return fmt.Errorf("branch does not belong to project")
	}
	
	if branch.Status != BranchStatusActive {
		return fmt.Errorf("cannot switch to inactive branch: %s", branch.Status)
	}
	
	// Verify branch collections exist
	branchDB := NewBranchDatabase(s.client, "argon", branchID.Hex())
	collections, err := branchDB.ListBranchCollections(ctx)
	if err != nil {
		return fmt.Errorf("failed to verify branch collections: %w", err)
	}
	
	log.Printf("Switching to branch %s (%s) with %d collections", 
		branch.Name, branchID.Hex(), len(collections))
	
	// For now, switching is handled by the client creating a new service instance
	// with the branch context. In a full implementation, this might also:
	// 1. Update a global branch registry
	// 2. Notify change stream processors
	// 3. Update connection routing tables
	// 4. Handle pending transactions
	
	// Update branch last accessed time
	collection := s.db.Collection("branches")
	_, err = collection.UpdateOne(ctx, 
		bson.M{"_id": branchID}, 
		bson.M{"$set": bson.M{
			"last_accessed_at": time.Now(),
			"updated_at": time.Now(),
		}})
	
	if err != nil {
		log.Printf("Warning: failed to update branch access time: %v", err)
	}
	
	log.Printf("Successfully switched to branch %s", branch.Name)
	return nil
}

// GetActiveBranch returns the currently active branch for a project
func (s *Service) GetActiveBranch(ctx context.Context, projectID primitive.ObjectID) (*Branch, error) {
	// This is a placeholder implementation
	// In a real system, this would track the active branch per session/connection
	
	// For now, return the main branch as default
	branches, err := s.ListBranches(ctx, projectID)
	if err != nil {
		return nil, err
	}
	
	// Find main branch
	for _, branch := range branches {
		if branch.IsMain && branch.Status == BranchStatusActive {
			return branch, nil
		}
	}
	
	return nil, fmt.Errorf("no active main branch found for project")
}

// GetBranchStats returns statistics about a branch
func (s *Service) GetBranchStats(ctx context.Context, branchID primitive.ObjectID) (*BranchStats, error) {
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return nil, err
	}
	
	// Get change count
	changeCollection := s.db.Collection("change_events")
	changeCount, err := changeCollection.CountDocuments(ctx, bson.M{"branch_id": branchID})
	if err != nil {
		return nil, fmt.Errorf("failed to count changes: %w", err)
	}
	
	// Get last change timestamp
	var lastChange ChangeEvent
	opts := options.FindOne().SetSort(bson.D{{Key: "timestamp", Value: -1}})
	err = changeCollection.FindOne(ctx, bson.M{"branch_id": branchID}, opts).Decode(&lastChange)
	
	var lastChangeAt *time.Time
	if err == nil {
		lastChangeAt = &lastChange.Timestamp
	}
	
	return &BranchStats{
		DocumentCount:    branch.DocumentCount,
		StorageSize:      branch.StorageSize,
		ChangeCount:      changeCount,
		LastChangeAt:     lastChangeAt,
		CompressionRatio: calculateCompressionRatio(branch.StorageSize),
	}, nil
}

// Helper functions

func (s *Service) copyFromParent(ctx context.Context, branch *Branch, parentID primitive.ObjectID) error {
	// Get parent branch information
	parentBranch, err := s.GetBranch(ctx, parentID)
	if err != nil {
		return fmt.Errorf("failed to get parent branch: %w", err)
	}
	
	// Create branch databases for parent and child
	parentBranchDB := NewBranchDatabase(s.client, "argon", parentBranch.ID.Hex())
	childBranchDB := NewBranchDatabase(s.client, "argon", branch.ID.Hex())
	
	// Get all collections from parent branch that should be copied
	collections, err := s.getDataCollections(ctx, parentBranchDB)
	if err != nil {
		return fmt.Errorf("failed to get parent collections: %w", err)
	}
	
	log.Printf("Copying %d collections from parent branch %s to new branch %s", 
		len(collections), parentBranch.Name, branch.Name)
	
	// Copy collections from parent to child branch
	if err := childBranchDB.CreateBranchCollections(ctx, parentBranchDB, collections); err != nil {
		return fmt.Errorf("failed to create branch collections: %w", err)
	}
	
	// Update branch metadata with copy information
	branch.Metadata["parent_copied_at"] = time.Now()
	branch.Metadata["parent_collections_copied"] = len(collections)
	branch.Metadata["collections"] = collections
	
	// Update branch document with metadata
	collection := s.db.Collection("branches")
	_, err = collection.UpdateOne(ctx, 
		bson.M{"_id": branch.ID}, 
		bson.M{"$set": bson.M{
			"metadata": branch.Metadata,
			"document_count": calculateTotalDocuments(collections, parentBranchDB, ctx),
		}})
	
	if err != nil {
		return fmt.Errorf("failed to update branch metadata: %w", err)
	}
	
	log.Printf("Successfully copied %d collections from parent branch %s", len(collections), parentBranch.Name)
	return nil
}

// getDataCollections returns all data collections (non-metadata) that should be copied
func (s *Service) getDataCollections(ctx context.Context, branchDB *BranchDatabase) ([]string, error) {
	// Get all collections in the database
	db := branchDB.Database()
	cursor, err := db.ListCollections(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	var collections []string
	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			continue
		}
		
		if name, ok := result["name"].(string); ok {
			// Only include data collections (skip metadata collections)
			if !isMetadataCollection(name) && branchDB.belongsToBranch(name) {
				// Get the base collection name (without branch prefix)
				baseName := s.getBaseCollectionName(name, branchDB)
				collections = append(collections, baseName)
			}
		}
	}
	
	// If no collections found, create some default collections for demo
	if len(collections) == 0 {
		collections = []string{"users", "products", "orders"}
		log.Printf("No existing collections found, will create default collections: %v", collections)
	}
	
	return collections, nil
}

// getBaseCollectionName extracts the base collection name from a potentially prefixed name
func (s *Service) getBaseCollectionName(collectionName string, branchDB *BranchDatabase) string {
	// If this is a prefixed collection, extract the base name
	if branchDB.prefix != "" && strings.HasPrefix(collectionName, branchDB.prefix+"_") {
		return strings.TrimPrefix(collectionName, branchDB.prefix+"_")
	}
	return collectionName
}

// calculateTotalDocuments counts documents across all collections (for metadata)
func calculateTotalDocuments(collections []string, branchDB *BranchDatabase, ctx context.Context) int64 {
	total := int64(0)
	for _, collectionName := range collections {
		collection := branchDB.Collection(collectionName)
		count, err := collection.CountDocuments(ctx, bson.M{})
		if err == nil {
			total += count
		}
	}
	return total
}

func generateRevision() string {
	return primitive.NewObjectID().Hex()
}

func calculateCompressionRatio(storageSize int64) float64 {
	// Placeholder - in real implementation, compare compressed vs uncompressed size
	if storageSize == 0 {
		return 0
	}
	return 0.7 // Assume 70% compression ratio
}

// initializeBranchStorage creates the initial storage structure for a branch
func (s *Service) initializeBranchStorage(ctx context.Context, branch *Branch) error {
	// Create initial metadata for the branch in storage
	branchMetadata := map[string]interface{}{
		"id":          branch.ID.Hex(),
		"name":        branch.Name,
		"project_id":  branch.ProjectID.Hex(),
		"created_at":  branch.CreatedAt,
		"is_main":     branch.IsMain,
		"parent":      branch.ParentBranch,
		"storage_version": "1.0",
	}
	
	// Store initial metadata
	metadataPath := fmt.Sprintf("%s/metadata.json", branch.StoragePath)
	metadataBytes, err := json.Marshal(branchMetadata)
	if err != nil {
		return fmt.Errorf("failed to marshal branch metadata: %w", err)
	}
	
	if err := s.storage.Upload(metadataPath, metadataBytes); err != nil {
		return fmt.Errorf("failed to upload branch metadata: %w", err)
	}
	
	// Create initial empty delta index
	deltaIndex := map[string]interface{}{
		"version":    "1.0",
		"branch_id":  branch.ID.Hex(),
		"deltas":     []string{}, // Empty initially
		"created_at": time.Now(),
	}
	
	deltaIndexPath := fmt.Sprintf("%s/delta_index.json", branch.StoragePath)
	deltaIndexBytes, err := json.Marshal(deltaIndex)
	if err != nil {
		return fmt.Errorf("failed to marshal delta index: %w", err)
	}
	
	if err := s.storage.Upload(deltaIndexPath, deltaIndexBytes); err != nil {
		return fmt.Errorf("failed to upload delta index: %w", err)
	}
	
	return nil
}

// StoreBranchChanges stores changes for a branch using the storage engine
func (s *Service) StoreBranchChanges(ctx context.Context, branchID primitive.ObjectID, changes []ChangeEvent) error {
	// Convert change events to delta operations
	operations := make([]storage.DeltaOperation, len(changes))
	for i, change := range changes {
		operations[i] = storage.DeltaOperation{
			ID:            primitive.NewObjectID().Hex(),
			OperationType: change.OperationType,
			Collection:    change.Collection,
			DocumentID:    change.DocumentID,
			FullDocument:  change.FullDocument,
			Timestamp:     change.Timestamp,
			ResumeToken:   change.ResumeToken,
		}
	}
	
	// Get branch to get project ID
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return fmt.Errorf("failed to get branch: %w", err)
	}
	
	// Store delta using storage service
	deltaPath, err := s.storage.StoreDelta(branchID.Hex(), branch.ProjectID.Hex(), operations)
	if err != nil {
		return fmt.Errorf("failed to store delta: %w", err)
	}
	
	// Update branch metadata with new delta
	update := bson.M{
		"$inc": bson.M{
			"document_count": len(changes),
		},
		"$set": bson.M{
			"updated_at":        time.Now(),
			"current_revision":  generateRevision(),
			"last_sync_at":      time.Now(),
		},
		"$push": bson.M{
			"metadata.deltas": deltaPath,
		},
	}
	
	collection := s.db.Collection("branches")
	_, err = collection.UpdateOne(ctx, bson.M{"_id": branchID}, update)
	if err != nil {
		return fmt.Errorf("failed to update branch metadata: %w", err)
	}
	
	return nil
}

// GetBranchStorageStats returns storage statistics for a branch
func (s *Service) GetBranchStorageStats(ctx context.Context, branchID primitive.ObjectID) (*BranchStorageStats, error) {
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return nil, err
	}
	
	// Get delta files for the branch
	deltas, err := s.storage.ListDeltas(branch.ProjectID.Hex(), branchID.Hex())
	if err != nil {
		return nil, fmt.Errorf("failed to list deltas: %w", err)
	}
	
	var totalSize int64
	var totalCompressedSize int64
	var totalOperations int64
	
	// Calculate storage statistics from deltas
	for _, deltaPath := range deltas {
		delta, err := s.storage.LoadDelta(deltaPath)
		if err != nil {
			continue // Skip corrupted deltas
		}
		
		totalSize += delta.Metadata.UncompressedSize
		totalCompressedSize += delta.Metadata.CompressedSize
		totalOperations += int64(delta.Metadata.OperationCount)
	}
	
	compressionRatio := float64(0)
	if totalSize > 0 {
		compressionRatio = float64(totalCompressedSize) / float64(totalSize)
	}
	
	return &BranchStorageStats{
		BranchID:           branchID.Hex(),
		DeltaCount:         int64(len(deltas)),
		UncompressedSize:   totalSize,
		CompressedSize:     totalCompressedSize,
		CompressionRatio:   compressionRatio,
		TotalOperations:    totalOperations,
		StoragePath:        branch.StoragePath,
		LastSyncAt:         branch.LastSyncAt,
	}, nil
}