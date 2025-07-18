package branch

import (
	"context"
	"fmt"
	"time"

	"argon/engine/internal/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Service struct {
	client  *mongo.Client
	db      *mongo.Database
	storage storage.Service
}

func NewService(client *mongo.Client, storage storage.Service) *Service {
	return &Service{
		client:  client,
		db:      client.Database("argon"),
		storage: storage,
	}
}

// CreateBranch creates a new database branch with copy-on-write semantics
func (s *Service) CreateBranch(ctx context.Context, req *BranchCreateRequest) (*Branch, error) {
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
	
	// If this has a parent branch, copy initial data
	if req.ParentBranch != nil {
		if err := s.copyFromParent(ctx, branch, *req.ParentBranch); err != nil {
			// Rollback branch creation
			collection.DeleteOne(ctx, bson.M{"_id": branch.ID})
			return nil, fmt.Errorf("failed to copy from parent branch: %w", err)
		}
	}
	
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

// DeleteBranch deletes a branch (soft delete by marking as archived)
func (s *Service) DeleteBranch(ctx context.Context, branchID primitive.ObjectID) error {
	collection := s.db.Collection("branches")
	
	update := bson.M{
		"status":     BranchStatusArchived,
		"updated_at": time.Now(),
	}
	
	_, err := collection.UpdateOne(ctx, bson.M{"_id": branchID}, bson.M{"$set": update})
	if err != nil {
		return fmt.Errorf("failed to delete branch: %w", err)
	}
	
	return nil
}

// SwitchBranch switches the active branch for a project
func (s *Service) SwitchBranch(ctx context.Context, projectID, branchID primitive.ObjectID) error {
	// This is a metadata operation - actual data switching happens at the application level
	// For now, we just verify the branch exists and is active
	branch, err := s.GetBranch(ctx, branchID)
	if err != nil {
		return err
	}
	
	if branch.ProjectID != projectID {
		return fmt.Errorf("branch does not belong to project")
	}
	
	if branch.Status != BranchStatusActive {
		return fmt.Errorf("cannot switch to inactive branch")
	}
	
	// In a full implementation, we would:
	// 1. Update connection strings/routing
	// 2. Ensure data consistency
	// 3. Handle any pending changes
	
	return nil
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
	// In a full implementation, this would:
	// 1. Copy metadata pointers from parent
	// 2. Set up copy-on-write references
	// 3. Initialize change tracking
	
	// For now, just create a reference
	branch.Metadata["parent_copied_at"] = time.Now()
	
	collection := s.db.Collection("branches")
	_, err := collection.UpdateOne(ctx, 
		bson.M{"_id": branch.ID}, 
		bson.M{"$set": bson.M{"metadata": branch.Metadata}})
	
	return err
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