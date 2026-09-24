package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/importer"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func importRecoveryFixture(t *testing.T) (*importer.ImportService, *projectwal.ProjectService, *mongo.Database, *mongo.Database) {
	t.Helper()
	meta := setupTestDB(t)
	source := setupTestSourceDB(t, fmt.Sprintf("argon_import_recovery_%d", time.Now().UnixNano()))
	log, err := wal.NewService(meta)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(meta, log)
	require.NoError(t, err)
	projects, err := projectwal.NewProjectService(meta, log, branches)
	require.NoError(t, err)
	return importer.NewImportService(log, projects, branches), projects, meta, source
}

func TestImport_PreflightAndQuiescenceFailBeforeTargetCreation(t *testing.T) {
	service, projects, meta, source := importRecoveryFixture(t)
	ctx := context.Background()
	opts := importer.ImportOptions{MongoURI: getTestMongoURI(), DatabaseName: source.Name(), ProjectName: "preflight"}
	// While multiple collections are being written, a caller must not begin
	// copying without the explicit stopped-writers acknowledgment.
	for _, collection := range []string{"orders", "payments"} {
		_, err := source.Collection(collection).InsertOne(ctx, bson.M{"_id": 1})
		require.NoError(t, err)
	}
	writerCtx, stopWriter := context.WithCancel(ctx)
	defer stopWriter()
	writeDone := make(chan error, 1)
	firstWrite := make(chan struct{})
	go func() {
		first := true
		for writerCtx.Err() == nil {
			for _, name := range []string{"orders", "payments"} {
				if _, err := source.Collection(name).UpdateOne(writerCtx, bson.M{"_id": 1}, bson.M{"$inc": bson.M{"revision": 1}}); err != nil {
					writeDone <- err
					return
				}
			}
			if first {
				close(firstWrite)
				first = false
			}
		}
		writeDone <- writerCtx.Err()
	}()
	select {
	case <-firstWrite:
	case err := <-writeDone:
		t.Fatalf("source writer: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("source writer did not start")
	}
	_, err := service.ImportDatabase(ctx, opts)
	require.ErrorContains(t, err, "source_quiesced")
	stopWriter()
	<-writeDone
	n, err := meta.Collection("wal_projects").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.Zero(t, n)

	opts.SourceQuiesced = true
	opts.DatabaseName = "argon_missing_import_source"
	_, err = service.ImportDatabase(ctx, opts)
	require.ErrorContains(t, err, "no importable collections")
	opts.DatabaseName = source.Name()
	require.NoError(t, source.CreateView(ctx, "orders_view", "orders", mongo.Pipeline{}))
	_, err = service.ImportDatabase(ctx, opts)
	require.ErrorContains(t, err, "unsupported type")
	require.NoError(t, source.Collection("orders_view").Drop(ctx))
	result, err := service.ImportDatabase(ctx, opts)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.ImportedDocs)
	_, err = projects.GetProjectByName(opts.ProjectName)
	require.NoError(t, err)
	require.Error(t, projects.CleanupImport(ctx, opts.ProjectName), "cleanup must never delete a completed project")
}

func TestImport_CanceledCopyIsHiddenCleanedAndRetryable(t *testing.T) {
	service, projects, meta, source := importRecoveryFixture(t)
	ctx := context.Background()
	docs := make([]interface{}, 1500)
	for i := range docs {
		docs[i] = bson.M{"_id": i, "value": "source"}
	}
	_, err := source.Collection("records").InsertMany(ctx, docs)
	require.NoError(t, err)
	opts := importer.ImportOptions{MongoURI: getTestMongoURI(), DatabaseName: source.Name(), ProjectName: "retry", BatchSize: 1, SourceQuiesced: true}
	copyCtx, cancelCopy := context.WithCancel(ctx)
	defer cancelCopy()
	done := make(chan error, 1)
	go func() { _, err := service.ImportDatabase(copyCtx, opts); done <- err }()
	var staged wal.Project
	require.Eventually(t, func() bool {
		if err := meta.Collection("wal_projects").FindOne(ctx, bson.M{"name": "retry", "importing": true}).Decode(&staged); err != nil {
			return false
		}
		n, _ := meta.Collection("wal_log").CountDocuments(ctx, bson.M{"project_id": staged.ID, "actor": "importer"})
		return n > 0
	}, 15*time.Second, 5*time.Millisecond)
	_, err = projects.GetProjectByName("retry")
	require.Error(t, err, "in-progress data must not be published")
	visible, err := projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, visible)
	cancelCopy()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled import did not finish cleanup")
	}
	for _, name := range []string{"wal_projects", "wal_log", "wal_branches", "wal_counters"} {
		n, err := meta.Collection(name).CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.Zero(t, n, "%s retains failed import data", name)
	}
	opts.BatchSize = 500
	result, err := service.ImportDatabase(ctx, opts)
	require.NoError(t, err)
	require.EqualValues(t, len(docs), result.ImportedDocs)
}

func TestImport_CrashRecoveryOnlyRemovesStagedProject(t *testing.T) {
	_, projects, meta, _ := importRecoveryFixture(t)
	ctx := context.Background()
	ordinary, err := projects.CreateProject("keep")
	require.NoError(t, err)
	staged, err := projects.BeginImport(ctx, "crashed")
	require.NoError(t, err)
	// Simulate a process disappearing: no importer defer runs. A separate
	// recovery call may delete only this explicitly unfinished reservation.
	require.NoError(t, projects.CleanupImport(ctx, "crashed"))
	n, err := meta.Collection("wal_log").CountDocuments(ctx, bson.M{"project_id": staged.ID})
	require.NoError(t, err)
	require.Zero(t, n)
	require.Error(t, projects.AbortImport(ctx, ordinary.ID))
	_, err = projects.GetProjectByName("keep")
	require.NoError(t, err)
	_, err = projects.BeginImport(ctx, "crashed")
	require.NoError(t, err, "crash recovery must free the original name")
}
