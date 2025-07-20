package wal

import (
	"context"
	"errors"
	"fmt"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ProjectService manages WAL-enabled projects
type ProjectService struct {
	db         *mongo.Database
	collection *mongo.Collection
	wal        *wal.Service
	branches   *branchwal.BranchService
}

// NewProjectService creates a new WAL project service
func NewProjectService(db *mongo.Database, walService *wal.Service, branchService *branchwal.BranchService) (*ProjectService, error) {
	s := &ProjectService{
		db:         db,
		collection: db.Collection("wal_projects"),
		wal:        walService,
		branches:   branchService,
	}

	// Create indexes
	ctx := context.Background()
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.M{"name": 1},
			Options: options.Index().SetUnique(true),
		},
	}

	_, err := s.collection.Indexes().CreateMany(ctx, indexes)
	if err != nil {
		return nil, fmt.Errorf("failed to create indexes: %w", err)
	}

	return s, nil
}

// CreateProject creates a new WAL-enabled project
func (s *ProjectService) CreateProject(name string) (*wal.Project, error) {
	ctx := context.Background()

	// Check if project already exists
	existing, _ := s.GetProjectByName(name)
	if existing != nil {
		return nil, errors.New("project already exists")
	}

	projectID := primitive.NewObjectID().Hex()

	// Create WAL entry for project creation
	entry := &wal.Entry{
		ProjectID: projectID,
		Operation: wal.OpCreateProject,
		Metadata: map[string]interface{}{
			"project_name": name,
		},
	}

	_, err := s.wal.Append(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Create main branch
	mainBranch, err := s.branches.CreateBranch(projectID, "main", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create main branch: %w", err)
	}

	// Create project record
	project := &wal.Project{
		ID:           projectID,
		Name:         name,
		MainBranchID: mainBranch.ID,
		CreatedAt:    time.Now(),
		UseWAL:       true,
	}

	// Insert project record
	_, err = s.collection.InsertOne(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	return project, nil
}

// GetProject retrieves a project by ID
func (s *ProjectService) GetProject(projectID string) (*wal.Project, error) {
	ctx := context.Background()
	var project wal.Project

	err := s.collection.FindOne(ctx, bson.M{"_id": projectID}).Decode(&project)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errors.New("project not found")
		}
		return nil, err
	}

	return &project, nil
}

// GetProjectByName retrieves a project by name
func (s *ProjectService) GetProjectByName(name string) (*wal.Project, error) {
	ctx := context.Background()
	var project wal.Project

	err := s.collection.FindOne(ctx, bson.M{"name": name}).Decode(&project)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errors.New("project not found")
		}
		return nil, err
	}

	return &project, nil
}

// ListProjects lists all WAL-enabled projects
func (s *ProjectService) ListProjects() ([]*wal.Project, error) {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{"use_wal": true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var projects []*wal.Project
	if err := cursor.All(ctx, &projects); err != nil {
		return nil, err
	}

	return projects, nil
}

// DeleteProject deletes a project and all its branches
func (s *ProjectService) DeleteProject(projectID string) error {
	ctx := context.Background()

	// Get project
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}

	// Get all branches
	branches, err := s.branches.ListBranches(projectID)
	if err != nil {
		return err
	}

	// Delete all branches except main
	for _, branch := range branches {
		if branch.Name != "main" {
			if err := s.branches.DeleteBranch(projectID, branch.Name); err != nil {
				// Log error but continue
				fmt.Printf("Failed to delete branch %s: %v\n", branch.Name, err)
			}
		}
	}

	// Create WAL entry for project deletion
	entry := &wal.Entry{
		ProjectID: projectID,
		Operation: wal.OpDeleteProject,
		Metadata: map[string]interface{}{
			"project_name": project.Name,
		},
	}

	_, err = s.wal.Append(entry)
	if err != nil {
		return fmt.Errorf("failed to append WAL entry: %w", err)
	}

	// Force delete main branch (special case for project deletion)
	if err := s.branches.ForceDeleteBranch(projectID, "main"); err != nil {
		fmt.Printf("Failed to delete main branch: %v\n", err)
	}

	// Delete project record
	_, err = s.collection.DeleteOne(ctx, bson.M{"_id": projectID})
	return err
}