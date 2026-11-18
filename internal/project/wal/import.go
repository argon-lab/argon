package wal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

const projectCollectionName = "wal_projects"

// CheckImportTarget checks for a published project without creating any metadata.
// BeginImport still reserves the name and detects concurrent or unpublished imports.
func CheckImportTarget(ctx context.Context, db *mongo.Database, name string) error {
	err := db.Collection(projectCollectionName).FindOne(ctx, bson.M{"name": name, "importing": bson.M{"$ne": true}}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check import project name: %w", err)
	}
	return fmt.Errorf("project '%s' already exists", name)
}

// BeginImport reserves a project name before any copying. Staged imports are
// hidden from normal project lookup/listing until CompleteImport publishes them.
func (s *ProjectService) BeginImport(ctx context.Context, name string) (_ *wal.Project, err error) {
	project := &wal.Project{ID: primitive.NewObjectID().Hex(), Name: name, CreatedAt: time.Now(), UseWAL: true, Importing: true}
	if _, err = s.collection.InsertOne(ctx, project); err != nil {
		return nil, fmt.Errorf("reserve import project: %w", err)
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if cleanupErr := s.AbortImport(cleanup, project.ID); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup incomplete for import project %s: %w", name, cleanupErr))
			}
		}
	}()
	if _, err = s.wal.Append(&wal.Entry{ProjectID: project.ID, Operation: wal.OpCreateProject,
		Metadata: map[string]interface{}{"project_name": name, "import": true}}); err != nil {
		return nil, err
	}
	branch, err := s.branches.CreateBranch(project.ID, "main", "")
	if err != nil {
		return nil, err
	}
	project.MainBranchID = branch.ID
	_, err = s.collection.UpdateOne(ctx, bson.M{"_id": project.ID, "importing": true}, bson.M{"$set": bson.M{"main_branch_id": branch.ID}})
	return project, err
}

func (s *ProjectService) CompleteImport(ctx context.Context, projectID string) error {
	result, err := s.collection.UpdateOne(ctx, bson.M{"_id": projectID, "importing": true}, bson.M{"$unset": bson.M{"importing": ""}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("staged import %s no longer exists", projectID)
	}
	return nil
}

// AbortImport removes only an unpublished import and its private history in one
// transaction. It cannot delete an ordinary or successfully published project.
func (s *ProjectService) AbortImport(ctx context.Context, projectID string) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	var removed []string
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		removed = nil
		result, err := s.collection.DeleteOne(sc, bson.M{"_id": projectID, "importing": true})
		if err != nil {
			return nil, err
		}
		if result.DeletedCount != 1 {
			return nil, fmt.Errorf("refusing cleanup: project is not an unpublished import")
		}
		// An online import also left a source binding and a stream position
		// for its main branch. Like everything else here these are rows in
		// the metadata database; the source itself is never touched.
		branchIDs, err := s.db.Collection("wal_branches").Distinct(sc, "_id", bson.M{"project_id": projectID})
		if err != nil {
			return nil, err
		}
		if len(branchIDs) > 0 {
			if _, err := s.db.Collection("wal_ingest_state").DeleteMany(sc, bson.M{"_id": bson.M{"$in": branchIDs}}); err != nil {
				return nil, err
			}
		}
		for _, id := range branchIDs {
			if text, ok := id.(string); ok {
				removed = append(removed, text)
			}
		}
		for _, name := range []string{"wal_log", "wal_branches", "wal_sources"} {
			if _, err := s.db.Collection(name).DeleteMany(sc, bson.M{"project_id": projectID}); err != nil {
				return nil, err
			}
		}
		_, err = s.db.Collection("wal_counters").DeleteOne(sc, bson.M{"_id": projectID})
		return nil, err
	})
	if err != nil {
		return err
	}
	// Snapshots of the unpublished branches hold a copy of the imported
	// data. They are removed outside the transaction: their cleanup takes
	// the publication lock and may reach an external chunk store.
	if s.afterImportAbort != nil && len(removed) > 0 {
		if err := s.afterImportAbort(ctx, removed); err != nil {
			return fmt.Errorf("the unfinished import was removed, but its snapshots were not: %w", err)
		}
	}
	return nil
}

// CleanupImport is an explicit recovery operation after a killed/crashed import.
// The operator must stop the original importer before calling it.
func (s *ProjectService) CleanupImport(ctx context.Context, name string) error {
	var project wal.Project
	if err := s.collection.FindOne(ctx, bson.M{"name": name, "importing": true}).Decode(&project); err != nil {
		return fmt.Errorf("no unfinished import named %q: %w", name, err)
	}
	return s.AbortImport(ctx, project.ID)
}
