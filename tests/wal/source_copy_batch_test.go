package wal_test

import (
	"context"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type importInitHook struct {
	importer.Follower
	afterInit func(branchID string)
}

func (h importInitHook) InitSourceState(ctx context.Context, branchID string) error {
	if err := h.Follower.InitSourceState(ctx, branchID); err != nil {
		return err
	}
	h.afterInit(branchID)
	return nil
}

func TestOnlineImport_RejectsForeignHeadBeforeFirstCopyBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)

	f.services.Importer.EnableOnline(f.services.Client.Database(f.metadataName), f.services.Sources, importInitHook{
		Follower: f.services.Ingest,
		afterInit: func(branchID string) {
			branch, err := f.services.Branches.GetBranchByID(branchID)
			require.NoError(t, err)
			require.Equal(t, branch.CreatedLSN, branch.HeadLSN)
			appendLegacyCatalogPut(t, f, branch, "foreign")
		},
	})
	result, err := f.services.Importer.ImportDatabase(ctx, importer.ImportOptions{
		Online: true, MongoURI: f.sourceURI, DatabaseName: f.source.Name(), ProjectName: f.project,
	})
	require.ErrorIs(t, err, branchwal.ErrBranchChanged)
	require.Nil(t, result)
	projects, err := f.services.Projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)
}

func TestOnlineImport_RejectsForeignHeadBetweenCopyBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)

	// Only the copy uses this WAL client. When it reserves LSNs for its
	// second batch, the first batch's head is already visible.
	var batches int
	var copiedHead, foreignHead int64
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName != "findAndModify" || e.Command.Lookup("findAndModify").StringValue() != "wal_counters" {
			return
		}
		batches++
		if batches == 2 {
			projectID := e.Command.Lookup("query", "_id").StringValue()
			branch, err := f.services.Branches.GetBranch(projectID, "main")
			require.NoError(t, err)
			require.Greater(t, branch.HeadLSN, branch.CreatedLSN)
			copiedHead = branch.HeadLSN
			foreignHead = appendLegacyCatalogPut(t, f, branch, "foreign")
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	metadata := client.Database(f.metadataName)
	copyWAL, err := wal.NewService(metadata)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(metadata, copyWAL)
	require.NoError(t, err)
	copyService := importer.NewImportService(copyWAL, f.services.Projects, branches)
	copyService.EnableOnline(metadata, f.services.Sources, f.services.Ingest)

	result, err := copyService.ImportDatabase(ctx, importer.ImportOptions{
		Online: true, MongoURI: f.sourceURI, DatabaseName: f.source.Name(), ProjectName: f.project, BatchSize: 1,
	})
	require.Greater(t, foreignHead, copiedHead)
	require.ErrorIs(t, err, branchwal.ErrBranchChanged)
	require.Nil(t, result)
	projects, err := f.services.Projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)
}
