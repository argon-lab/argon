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

// CreateProject creates a new WAL-enabled project. The project row goes in
// last, so a project is never visible without its main branch, and the
// unique name decides among creates that arrive together. A create that
// loses there, or fails earlier, removes the record and main branch it made:
// they belong to a project ID nothing else has seen.
func (s *ProjectService) CreateProject(name string) (*wal.Project, error) {
	ctx := context.Background()

	if existing, _ := s.GetProjectByName(name); existing != nil {
		return nil, wal.ErrProjectExists
	}

	projectID := primitive.NewObjectID().Hex()
	fail := func(cause error) (*wal.Project, error) {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.removeUnmade(cleanup, projectID); err != nil {
			return nil, errors.Join(cause, fmt.Errorf("what this create made for project %s (ID %s) could not be removed: %w", name, projectID, err))
		}
		return nil, cause
	}

	if _, err := s.wal.Append(&wal.Entry{
		ProjectID: projectID,
		Operation: wal.OpCreateProject,
		Metadata:  map[string]interface{}{"project_name": name},
	}); err != nil {
		return fail(fmt.Errorf("failed to append WAL entry: %w", err))
	}
	mainBranch, err := s.branches.CreateBranch(projectID, "main", "")
	if err != nil {
		return fail(fmt.Errorf("failed to create main branch: %w", err))
	}
	project := &wal.Project{
		ID:           projectID,
		Name:         name,
		MainBranchID: mainBranch.ID,
		CreatedAt:    time.Now(),
		UseWAL:       true,
	}
	_, err = s.collection.InsertOne(ctx, project)
	for tries := 0; mongo.IsDuplicateKeyError(err); tries++ {
		taken, free := s.nameTaken(ctx, name)
		if !free {
			return fail(taken)
		}
		// What held the name is gone again (an import that was aborted, or
		// a project deleted just now): the name is free, so insert again.
		if tries >= 2 {
			return fail(fmt.Errorf("project name %q was in use a moment ago and may be free now; try again", name))
		}
		_, err = s.collection.InsertOne(ctx, project)
	}
	if err != nil {
		return fail(fmt.Errorf("failed to create project: %w", err))
	}
	return project, nil
}

// nameTaken says why a project name cannot be used: a project has it, or an
// import that has not finished has reserved it. free reports that nothing
// holds the name any more.
func (s *ProjectService) nameTaken(ctx context.Context, name string) (taken error, free bool) {
	var holder wal.Project
	err := s.collection.FindOne(ctx, bson.M{"name": name}).Decode(&holder)
	switch {
	case err == mongo.ErrNoDocuments:
		return nil, true
	case err == nil && holder.Importing:
		return fmt.Errorf("project name %q is reserved by an import that has not finished; wait for it, or, if that import is no longer running, remove it with argon import cleanup --project %s --yes", name, name), false
	}
	return wal.ErrProjectExists, false
}

// removeUnmade removes the WAL records, branches and LSN counter of a
// project ID that never got a project row. It refuses an ID that has one.
func (s *ProjectService) removeUnmade(ctx context.Context, projectID string) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		rows, err := s.collection.CountDocuments(sc, bson.M{"_id": projectID})
		if err != nil {
			return nil, err
		}
		if rows != 0 {
			return nil, fmt.Errorf("refusing cleanup: project %s exists", projectID)
		}
		for _, name := range []string{"wal_log", "wal_branches"} {
			if _, err := s.db.Collection(name).DeleteMany(sc, bson.M{"project_id": projectID}); err != nil {
				return nil, err
			}
		}
		_, err = s.db.Collection("wal_counters").DeleteOne(sc, bson.M{"_id": projectID})
		return nil, err
	})
	return err
}

// GetProject retrieves a project by ID
func (s *ProjectService) GetProject(projectID string) (*wal.Project, error) {
	ctx := context.Background()
	var project wal.Project

	err := s.collection.FindOne(ctx, bson.M{"_id": projectID, "importing": bson.M{"$ne": true}}).Decode(&project)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, wal.ErrProjectNotFound
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
