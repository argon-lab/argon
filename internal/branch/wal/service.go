package wal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// BranchService manages WAL-based branches
type BranchService struct {
	db         *mongo.Database
	collection *mongo.Collection
	wal        *wal.Service
}

// NewBranchService creates a new WAL branch service
func NewBranchService(db *mongo.Database, walService *wal.Service) (*BranchService, error) {
	s := &BranchService{
		db:         db,
		collection: db.Collection("wal_branches"),
		wal:        walService,
	}

	// Create indexes
	ctx := context.Background()
	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "name", Value: 1},
			},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{
				{Key: "project_id", Value: 1},
				{Key: "is_deleted", Value: 1},
			},
		},
	}

	_, err := s.collection.Indexes().CreateMany(ctx, indexes)
	if err != nil {
		return nil, fmt.Errorf("failed to create indexes: %w", err)
	}

	return s, nil
}

// CreateBranch creates a new WAL-based branch
func (s *BranchService) CreateBranch(projectID, name, parentID string) (*wal.Branch, error) {
	ctx := context.Background()

	// Check if branch already exists
	existing, _ := s.GetBranch(projectID, name)
	if existing != nil {
		return nil, errors.New("branch already exists")
	}

	// Get parent branch if specified
	var parentBranch *wal.Branch
	if parentID != "" {
		var err error
		parentBranch, err = s.GetBranchByID(parentID)
		if err != nil {
			return nil, fmt.Errorf("parent branch not found: %w", err)
		}
	}

	// Create WAL entry for branch creation
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  name,
		Operation: wal.OpCreateBranch,
		Metadata: map[string]interface{}{
			"branch_name": name,
			"parent_id":   parentID,
		},
	}

	lsn, err := s.wal.Append(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Create branch record
	branch := &wal.Branch{
		ID:         primitive.NewObjectID().Hex(),
		ProjectID:  projectID,
		Name:       name,
		ParentID:   parentID,
		CreatedAt:  time.Now(),
		CreatedLSN: lsn,
		IsDeleted:  false,
	}

	// Set head and base LSN
	if parentBranch != nil {
		branch.HeadLSN = parentBranch.HeadLSN // Inherit parent's HEAD
		branch.BaseLSN = parentBranch.HeadLSN // Fork point
	} else {
		branch.HeadLSN = lsn // New branch starts at creation LSN
		branch.BaseLSN = 0   // No parent
	}

	// Insert branch record
	_, err = s.collection.InsertOne(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("failed to create branch: %w", err)
	}

	return branch, nil
}

// GetBranch retrieves a branch by project ID and name
func (s *BranchService) GetBranch(projectID, name string) (*wal.Branch, error) {
	ctx := context.Background()
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{
		"project_id": projectID,
		"name":       name,
		"is_deleted": false,
	}).Decode(&branch)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errors.New("branch not found")
		}
		return nil, err
	}

	return &branch, nil
}

// GetBranchByID retrieves a branch by its ID
func (s *BranchService) GetBranchByID(branchID string) (*wal.Branch, error) {
	ctx := context.Background()
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{
		"_id":        branchID,
		"is_deleted": false,
	}).Decode(&branch)

	if err != nil {
		return nil, err
	}

	return &branch, nil
}

// ListBranches lists all branches for a project
func (s *BranchService) ListBranches(projectID string) ([]*wal.Branch, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{
		"project_id": projectID,
		"is_deleted": false,
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// DeleteBranch deletes a branch (simple version - just removes the pointer)
func (s *BranchService) DeleteBranch(projectID, name string) error {
	ctx := context.Background()

	// Get branch
	branch, err := s.GetBranch(projectID, name)
	if err != nil {
		return err
	}

	// Validate it's not the main branch
	if branch.Name == "main" {
		return errors.New("cannot delete main branch")
	}

	// Check for child branches
	childCount, err := s.collection.CountDocuments(ctx, bson.M{
		"project_id": projectID,
		"parent_id":  branch.ID,
		"is_deleted": false,
	})
	if err != nil {
		return err
	}
	if childCount > 0 {
		return errors.New("cannot delete branch with active children")
	}

	// Create WAL entry for deletion
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  name,
		Operation: wal.OpDeleteBranch,
		Metadata: map[string]interface{}{
			"branch_id":   branch.ID,
			"final_lsn":   branch.HeadLSN,
		},
	}

	_, err = s.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Simple deletion - just mark as deleted
	_, err = s.collection.UpdateOne(ctx, 
		bson.M{"_id": branch.ID},
		bson.M{"$set": bson.M{"is_deleted": true}},
	)

	return err
}

// UpdateBranchHead updates the head LSN of a branch
func (s *BranchService) UpdateBranchHead(branchID string, newLSN int64) error {
	ctx := context.Background()
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": branchID},
		bson.M{"$set": bson.M{"head_lsn": newLSN}},
	)
	return err
}

// GetChildBranches returns all child branches of a given branch
func (s *BranchService) GetChildBranches(parentID string) ([]*wal.Branch, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{
		"parent_id":  parentID,
		"is_deleted": false,
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}