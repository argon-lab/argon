// Package pin implements dataset pins: named, immutable references to a
// branch state (branch, LSN) that stay materializable forever.
//
// A pin is to Argon what a tag is to Git — with one addition Git doesn't
// need: garbage collection reclaims WAL entries once snapshots cover them
// and the retention window has passed, and a pin punches a permanent hole
// in that policy. For a pin at LSN P, the branch's reclaim cutoff is
// clamped to the newest snapshot usable at read bound P — and to zero
// (nothing reclaimed) while no such snapshot exists. Deleting a branch
// with pins is refused, the same way deleting a branch with live children
// is.
//
// Pins are also stable across resets: a reset records a discarded range,
// and discarded ranges only apply to readers whose bound lies beyond the
// range — a pinned read at P keeps seeing the state that was pinned, the
// same rule that keeps pre-reset backup branches intact.
//
// The intended use is reproducible agent evaluation: pin the dataset
// branch when an eval suite is authored, then fork branches or TTL
// sandboxes from the pin for every run and get identical input state,
// regardless of what happened to the branch since.
package pin

import (
	"context"
	"errors"
	"fmt"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Pin is a named, immutable reference to a branch state.
type Pin struct {
	ID         string    `bson:"_id" json:"id"`
	ProjectID  string    `bson:"project_id" json:"project_id"`
	BranchID   string    `bson:"branch_id" json:"branch_id"`
	BranchName string    `bson:"branch_name" json:"branch_name"`
	Name       string    `bson:"name" json:"name"`
	LSN        int64     `bson:"lsn" json:"lsn"`
	Note       string    `bson:"note,omitempty" json:"note,omitempty"`
	CreatedAt  time.Time `bson:"created_at" json:"created_at"`
}

// Service manages pins.
type Service struct {
	collection *mongo.Collection
	branches   *branchwal.BranchService
}

// NewService creates the pin service and its indexes.
func NewService(db *mongo.Database, branches *branchwal.BranchService) (*Service, error) {
	s := &Service{
		collection: db.Collection("wal_pins"),
		branches:   branches,
	}
	_, err := s.collection.Indexes().CreateMany(context.Background(), []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "name", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{{Key: "branch_id", Value: 1}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pin indexes: %w", err)
	}
	return s, nil
}

// Create pins a branch at the given LSN under a project-unique name. An
// LSN of 0 pins the branch's current head. The LSN must lie on the
// branch's own segment (base to head) — the same bounds as branching from
// history.
func (s *Service) Create(projectID, branchID, name string, lsn int64, note string) (*Pin, error) {
	if name == "" {
		return nil, errors.New("pin name must not be empty")
	}
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("branch not found: %w", err)
	}
	if branch.ProjectID != projectID {
		return nil, fmt.Errorf("branch %s does not belong to project %s", branchID, projectID)
	}
	if lsn == 0 {
		lsn = branch.HeadLSN
	}
	if lsn < branch.BaseLSN || lsn > branch.HeadLSN {
		return nil, fmt.Errorf("pin LSN %d is outside branch range [%d, %d]",
			lsn, branch.BaseLSN, branch.HeadLSN)
	}

	pin := &Pin{
		ID:         primitive.NewObjectID().Hex(),
		ProjectID:  projectID,
		BranchID:   branch.ID,
		BranchName: branch.Name,
		Name:       name,
		LSN:        lsn,
		Note:       note,
		CreatedAt:  time.Now(),
	}
	if _, err := s.collection.InsertOne(context.Background(), pin); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("pin %q already exists in this project", name)
		}
		return nil, fmt.Errorf("failed to create pin: %w", err)
	}
	return pin, nil
}

// Get returns a pin by project and name.
func (s *Service) Get(projectID, name string) (*Pin, error) {
	var pin Pin
	err := s.collection.FindOne(context.Background(),
		bson.M{"project_id": projectID, "name": name}).Decode(&pin)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("pin %q not found", name)
		}
		return nil, err
	}
	return &pin, nil
}

// List returns a project's pins, oldest first.
func (s *Service) List(projectID string) ([]*Pin, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{"project_id": projectID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	pins := make([]*Pin, 0)
	if err := cursor.All(ctx, &pins); err != nil {
		return nil, err
	}
	return pins, nil
}

// Delete removes a pin. The history it protected becomes reclaimable on
// the next GC run.
func (s *Service) Delete(projectID, name string) error {
	res, err := s.collection.DeleteOne(context.Background(),
		bson.M{"project_id": projectID, "name": name})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fmt.Errorf("pin %q not found", name)
	}
	return nil
}

// LSNsForBranch returns the pinned LSNs on a branch — the GC service's
// pin-lookup hook.
func (s *Service) LSNsForBranch(branchID string) ([]int64, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{"branch_id": branchID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var pins []Pin
	if err := cursor.All(ctx, &pins); err != nil {
		return nil, err
	}
	lsns := make([]int64, len(pins))
	for i, p := range pins {
		lsns[i] = p.LSN
	}
	return lsns, nil
}

// RequireNoPins is the branch service's delete guard: deleting a branch
// that pins reference would orphan them.
func (s *Service) RequireNoPins(branchID string) error {
	count, err := s.collection.CountDocuments(context.Background(),
		bson.M{"branch_id": branchID})
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("branch has %d pin(s); delete them first (argon pin delete)", count)
	}
	return nil
}
