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

	// onDelete runs after a branch is successfully soft-deleted through
	// DeleteBranch (never after ForceDeleteBranch: force-deleted branches
	// may still anchor descendants' history). Used to reclaim snapshots
	// without this package depending on the snapshot package.
	onDelete func(branchID string)
}

// SetDeleteHook registers a callback invoked after a successful
// DeleteBranch.
func (s *BranchService) SetDeleteHook(hook func(branchID string)) {
	s.onDelete = hook
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

	// The branch ID is generated up front so the WAL entry can reference
	// it; data entries key on branch IDs, so control entries must too.
	branchID := primitive.NewObjectID().Hex()

	// Create WAL entry for branch creation
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  branchID,
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
		ID:         branchID,
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

// GetBranchByIDAny retrieves a branch by its ID regardless of deletion
// state. Ancestry resolution uses this: a force-deleted parent must still
// anchor its descendants' history, whose entries remain in the WAL.
func (s *BranchService) GetBranchByIDAny(branchID string) (*wal.Branch, error) {
	ctx := context.Background()
	var branch wal.Branch

	err := s.collection.FindOne(ctx, bson.M{"_id": branchID}).Decode(&branch)
	if err != nil {
		return nil, err
	}

	return &branch, nil
}

// SetExpiry stamps (or clears, with nil) a branch's sandbox TTL.
func (s *BranchService) SetExpiry(branchID string, expiresAt *time.Time) error {
	ctx := context.Background()
	update := bson.M{"$unset": bson.M{"expires_at": ""}}
	if expiresAt != nil {
		update = bson.M{"$set": bson.M{"expires_at": *expiresAt}}
	}
	_, err := s.collection.UpdateOne(ctx, bson.M{"_id": branchID}, update)
	return err
}

// SetCheckoutState records (or clears, with empty values) a branch's
// physical-database checkout.
func (s *BranchService) SetCheckoutState(branchID, physicalDB, state string, checkedOutLSN int64) error {
	ctx := context.Background()
	update := bson.M{}
	if physicalDB == "" && state == "" {
		update["$unset"] = bson.M{"physical_db": "", "state": "", "checked_out_lsn": ""}
	} else {
		update["$set"] = bson.M{
			"physical_db":     physicalDB,
			"state":           state,
			"checked_out_lsn": checkedOutLSN,
		}
	}
	_, err := s.collection.UpdateOne(ctx, bson.M{"_id": branchID}, update)
	return err
}

// AddDiscardedRange records an LSN window abandoned by a reset so that
// materialization skips it. The entries themselves stay in the WAL for
// audit and for time travel to points before the reset.
func (s *BranchService) AddDiscardedRange(branchID string, from, to int64) error {
	if from > to {
		return fmt.Errorf("invalid discarded range [%d, %d]", from, to)
	}
	ctx := context.Background()
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": branchID},
		bson.M{"$push": bson.M{"discarded_ranges": wal.LSNRange{From: from, To: to}}},
	)
	return err
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
	defer func() { _ = cursor.Close(ctx) }()

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// ListBranchesAny lists all branches for a project including deleted ones.
// Deleted branches can still anchor live descendants' history, so tools
// that walk every timeline (e.g. WAL migration) must see them.
func (s *BranchService) ListBranchesAny(projectID string) ([]*wal.Branch, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{"project_id": projectID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

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

	// Validate it's not the main branch (unless force is specified)
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
			"branch_id": branch.ID,
			"final_lsn": branch.HeadLSN,
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
	if err != nil {
		return err
	}

	// Safe because DeleteBranch refuses branches with children: nothing
	// can reach this branch's snapshots through an ancestry chain anymore.
	if s.onDelete != nil {
		s.onDelete(branch.ID)
	}

	return nil
}

// UpdateBranchHead advances the head LSN of a branch. $max keeps the head
// monotonic under concurrent writers: with $set, a writer holding a smaller
// LSN could land after one holding a larger LSN and move the head backwards,
// hiding already-written entries from materialization. To move a head
// backwards deliberately (restore/reset), use SetBranchHead.
func (s *BranchService) UpdateBranchHead(branchID string, newLSN int64) error {
	ctx := context.Background()
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": branchID},
		bson.M{"$max": bson.M{"head_lsn": newLSN}},
	)
	return err
}

// SetBranchHead sets the head LSN of a branch unconditionally. This is the
// restore/reset path: unlike UpdateBranchHead it can move a head backwards,
// so it must only be used when discarding history is the intent.
func (s *BranchService) SetBranchHead(branchID string, newLSN int64) error {
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
	defer func() { _ = cursor.Close(ctx) }()

	var branches []*wal.Branch
	if err := cursor.All(ctx, &branches); err != nil {
		return nil, err
	}

	return branches, nil
}

// ForceDeleteBranch deletes a branch without safety checks (for project deletion)
func (s *BranchService) ForceDeleteBranch(projectID, name string) error {
	ctx := context.Background()

	// Get branch
	branch, err := s.GetBranch(projectID, name)
	if err != nil {
		return err
	}

	// Create WAL entry for deletion
	entry := &wal.Entry{
		ProjectID: projectID,
		BranchID:  name,
		Operation: wal.OpDeleteBranch,
		Metadata: map[string]interface{}{
			"branch_id": branch.ID,
			"final_lsn": branch.HeadLSN,
			"forced":    true,
		},
	}

	_, err = s.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Mark as deleted
	_, err = s.collection.UpdateOne(ctx,
		bson.M{"_id": branch.ID},
		bson.M{"$set": bson.M{"is_deleted": true}},
	)

	return err
}

// CreateBranchWithData creates a branch with specific metadata
func (s *BranchService) CreateBranchWithData(branch *wal.Branch) error {
	ctx := context.Background()

	// Check if branch already exists
	existing, _ := s.GetBranch(branch.ProjectID, branch.Name)
	if existing != nil {
		return errors.New("branch already exists")
	}

	// Create WAL entry for branch creation
	entry := &wal.Entry{
		ProjectID: branch.ProjectID,
		BranchID:  branch.Name,
		Operation: wal.OpCreateBranch,
		Metadata: map[string]interface{}{
			"branch_name": branch.Name,
			"base_lsn":    branch.BaseLSN,
			"head_lsn":    branch.HeadLSN,
		},
	}

	_, err := s.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Set default values
	branch.IsDeleted = false
	if branch.CreatedAt.IsZero() {
		branch.CreatedAt = time.Now()
	}

	// Insert branch record
	_, err = s.collection.InsertOne(ctx, branch)
	if err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	return nil
}
