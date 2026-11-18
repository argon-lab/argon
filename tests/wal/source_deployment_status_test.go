package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestOnlineImport_DeploymentReadsPublishedCheckpoints(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var concern string
	wireRows := -1
	monitor := &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.CommandName == "aggregate" {
				concern, _ = e.Command.Lookup("readConcern", "level").StringValueOK()
			}
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if e.CommandName == "aggregate" {
				rows, err := e.Reply.Lookup("cursor", "firstBatch").Array().Values()
				require.NoError(t, err)
				wireRows = len(rows)
			}
		},
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	db := client.Database(f.metadataName)
	f.services.Importer.EnableOnline(f.services.Client.Database(f.metadataName), f.services.Sources, importInitHook{
		Follower: f.services.Ingest,
		afterInit: func(branchID string) {
			_, status, err := f.services.Ingest.CaptureStatusFor(ctx, branchID)
			require.NoError(t, err)
			require.Equal(t, ingest.ReasonImporting, status.Health.Reason)

			reports, healthy, err := walcli.InspectCaptureHealth(ctx, db)
			require.NoError(t, err)
			require.True(t, healthy)
			require.Empty(t, reports)
			require.Equal(t, "snapshot", concern)
			require.Zero(t, wireRows, "MongoDB must omit the staged branch")
		},
	})
	f.importOnline(t, walcli.OnlineImport{})
	reports, healthy, err := walcli.InspectCaptureHealth(ctx, db)
	require.NoError(t, err)
	require.True(t, healthy)
	require.Len(t, reports, 1)
	require.Equal(t, 1, wireRows)

	// A missing project still needs a deployment alert.
	main := f.main(t)
	_, err = db.Collection("wal_branches").UpdateByID(ctx, main.ID, bson.M{"$set": bson.M{"project_id": "missing"}})
	require.NoError(t, err)
	orphaned, err := ingest.ReadCaptureReports(ctx, db)
	require.NoError(t, err)
	require.Len(t, orphaned, 1)
	require.False(t, orphaned[0].Healthy)
	require.Equal(t, ingest.ReasonProjectMissing, orphaned[0].Status.Health.Reason)
}

func TestOnlineImport_DeploymentSkipsUndecodableCheckpoint(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := f.services.Client.Database(f.metadataName)
	f.services.Importer.EnableOnline(db, f.services.Sources, importInitHook{
		Follower: f.services.Ingest,
		afterInit: func(branchID string) {
			state := f.ingestState()
			_, err := state.UpdateByID(ctx, branchID, bson.M{"$set": bson.M{"capture_status.state": bson.M{}}})
			require.NoError(t, err)
			_, _, err = f.services.Ingest.CaptureStatusFor(ctx, branchID)
			require.ErrorContains(t, err, "state")

			reports, healthy, err := walcli.InspectCaptureHealth(ctx, db)
			require.NoError(t, err)
			require.Empty(t, reports)
			require.True(t, healthy)

			_, err = state.UpdateByID(ctx, branchID, bson.M{"$unset": bson.M{"capture_status": ""}})
			require.NoError(t, err)
		},
	})
	f.importOnline(t, walcli.OnlineImport{})
}
