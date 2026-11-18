package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func appendLegacyCatalogPut(t *testing.T, f *followFixture, branch *wal.Branch, id string) int64 {
	t.Helper()
	post, err := bson.Marshal(bson.D{{Key: "_id", Value: id}, {Key: "stock", Value: int32(-1)}})
	require.NoError(t, err)
	head, err := f.services.WAL.Append(&wal.Entry{
		ProjectID: branch.ProjectID, BranchID: branch.ID, Operation: wal.OpPut,
		Collection: "catalog", DocumentID: id, PostImage: post, Actor: "merge:old-binary",
	})
	require.NoError(t, err)
	require.NoError(t, f.services.Branches.UpdateBranchHead(branch.ID, head))
	return head
}

func TestResync_RejectsForeignHeadAtReplayHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)
	consistent := f.status(t).ConsistentLSN

	appendLegacyCatalogPut(t, f, branch, "item-01")
	require.ErrorContains(t, f.status(t).HealthError(), "without the follower")

	// All the keys fit in one batch. This callback runs after prune commits.
	var expectedHead, foreignHead int64
	var handoffStatus *walcli.SourceInfo
	result, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI,
		Progress: func(p walcli.ResyncProgress) {
			if p.Phase == "prune" && p.Collection == "catalog" {
				require.Zero(t, foreignHead)
				expectedHead = f.main(t).HeadLSN
				foreignHead = appendLegacyCatalogPut(t, f, branch, "item-00")
				handoffStatus = f.status(t)
			}
			f.tick(t)
		},
	})
	require.Greater(t, foreignHead, expectedHead)
	require.NotNil(t, handoffStatus)
	require.True(t, handoffStatus.ResyncUnfinished)
	mismatch := fmt.Sprintf("moved from LSN %d to %d without the follower", expectedHead, foreignHead)
	require.Contains(t, handoffStatus.Error, mismatch)
	require.ErrorContains(t, err, mismatch)
	require.Nil(t, result)

	after := f.status(t)
	require.True(t, after.ResyncUnfinished)
	require.Contains(t, after.Error, mismatch)
	require.Error(t, after.HealthError())
	require.Equal(t, foreignHead, after.HeadLSN)
	require.Equal(t, consistent, after.ConsistentLSN)
	require.Equal(t, int32(-1), f.mainState(t, foreignHead)["catalog"]["item-00"]["stock"])
}

type importReplayHook struct {
	importer.Follower
	beforeReplay func(branchID string, expectedHead int64)
}

func (h importReplayHook) CatchUp(ctx context.Context, branchID string, src *mongo.Database, startAt primitive.Timestamp, expectedHead, firstLSN int64, reason string, redact func(string) string) (*ingest.ConsistentPoint, error) {
	h.beforeReplay(branchID, expectedHead)
	return h.Follower.CatchUp(ctx, branchID, src, startAt, expectedHead, firstLSN, reason, redact)
}

func TestOnlineImport_RejectsForeignHeadAtReplayHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)

	var copiedHead, foreignHead int64
	f.services.Importer.EnableOnline(f.services.Client.Database(f.metadataName), f.services.Sources, importReplayHook{
		Follower: f.services.Ingest,
		beforeReplay: func(branchID string, expectedHead int64) {
			branch, err := f.services.Branches.GetBranchByID(branchID)
			require.NoError(t, err)
			require.Equal(t, branch.HeadLSN, expectedHead)
			copiedHead = expectedHead
			foreignHead = appendLegacyCatalogPut(t, f, branch, "item-00")
			f.tick(t)
		},
	})
	_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
		URI: f.sourceURI, Database: f.source.Name(), Project: f.project,
	})
	require.Greater(t, foreignHead, copiedHead)
	require.ErrorContains(t, err, fmt.Sprintf("moved from LSN %d to %d without the follower", copiedHead, foreignHead))
	require.ErrorContains(t, err, "Nothing was published")
	projects, err := f.services.Projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)
}
