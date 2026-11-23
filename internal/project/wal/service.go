package wal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
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

	// afterImportAbort receives the branch IDs of an unpublished import
	// whose rows were just removed. Used to reclaim what lives outside the
	// transaction that removes them, such as snapshots in a chunk store.
	afterImportAbort func(ctx context.Context, branchIDs []string) error
}

// SetImportAbortHook registers the reclaim that follows a removed import.
func (s *ProjectService) SetImportAbortHook(hook func(ctx context.Context, branchIDs []string) error) {
	s.afterImportAbort = hook
}

// NewProjectService creates a new WAL project service
func NewProjectService(db *mongo.Database, walService *wal.Service, branchService *branchwal.BranchService) (*ProjectService, error) {
	s := &ProjectService{
		db:         db,
		collection: db.Collection(projectCollectionName),
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

// CreateProject creates a new WAL-enabled project. The project row is
// inserted first, hidden as an import is until it is complete, so the
// unique name decides among creates that arrive together before any of them
// makes a main branch; a create that fails after that removes what it made.
func (s *ProjectService) CreateProject(name string) (_ *wal.Project, err error) {
	ctx := context.Background()

	if existing, _ := s.GetProjectByName(name); existing != nil {
		return nil, wal.ErrProjectExists
	}

	project := &wal.Project{ID: primitive.NewObjectID().Hex(), Name: name, CreatedAt: time.Now(), UseWAL: true, Importing: true}
	if _, err = s.collection.InsertOne(ctx, project); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Another create, or an import still in progress, has the name.
			return nil, wal.ErrProjectExists
		}
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if cleanupErr := s.AbortImport(cleanup, project.ID); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup incomplete for project %s: %w", name, cleanupErr))
			}
		}
	}()

	if _, err = s.wal.Append(&wal.Entry{
		ProjectID: project.ID,
		Operation: wal.OpCreateProject,
		Metadata:  map[string]interface{}{"project_name": name},
	}); err != nil {
		return nil, fmt.Errorf("failed to append WAL entry: %w", err)
	}
	mainBranch, err := s.branches.CreateBranch(project.ID, "main", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create main branch: %w", err)
	}
	// Publish the project with its main branch in one write.
	result, err := s.collection.UpdateOne(ctx, bson.M{"_id": project.ID, "importing": true},
		bson.M{"$set": bson.M{"main_branch_id": mainBranch.ID}, "$unset": bson.M{"importing": ""}})
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	if result.MatchedCount != 1 {
		err = fmt.Errorf("failed to create project: its reservation was removed while it was being created")
		return nil, err
	}
	project.MainBranchID = mainBranch.ID
	project.Importing = false
	return project, nil
}

// GetProject retrieves a project by ID
func (s *ProjectService) GetProject(projectID string) (*wal.Project, error) {
	ctx := context.Background()
	var project wal.Project

	err := s.collection.FindOne(ctx, bson.M{"_id": projectID, "importing": bson.M{"$ne": true}}).Decode(&project)
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

	err := s.collection.FindOne(ctx, bson.M{"name": name, "importing": bson.M{"$ne": true}}).Decode(&project)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errors.New("project not found")
		}
		return nil, err
	}

	return &project, nil
}

// GetProjectIDByName resolves a project name to its ID (the wire proxy's
// alias lookup interface).
func (s *ProjectService) GetProjectIDByName(name string) (string, error) {
	project, err := s.GetProjectByName(name)
	if err != nil {
		return "", err
	}
	return project.ID, nil
}

// ListProjects lists all WAL-enabled projects
func (s *ProjectService) ListProjects() ([]*wal.Project, error) {
	return s.ListProjectsContext(context.Background())
}

// ListProjectsContext lists WAL-enabled projects using the caller's context.
func (s *ProjectService) ListProjectsContext(ctx context.Context) ([]*wal.Project, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"use_wal": true, "importing": bson.M{"$ne": true}})
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
		if branch.SandboxUsername != "" {
			return fmt.Errorf("discard sandbox %q before deleting its project", branch.Name)
		}
	}
	for _, branch := range branches {
		if branch.Name != "main" {
			if err := s.branches.DeleteBranch(projectID, branch.Name); err != nil {
				// Log error but continue
				fmt.Fprintf(os.Stderr, "Failed to delete branch %s: %v\n", branch.Name, err)
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
		fmt.Fprintf(os.Stderr, "Failed to delete main branch: %v\n", err)
	}

	// Delete project record
	_, err = s.collection.DeleteOne(ctx, bson.M{"_id": projectID})
	return err
}
