package wal_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A time-series collection stops following and also prevents a resync from
// writing its marker. The original degraded status and write-back refusal
// survive both attempts to resync.
func TestResync_FailedPreflightPreservesDegradedState(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)

	info, sandboxDB := f.agentSandbox(t, "catalog-edits")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)
	require.NoError(t, stop())

	require.NoError(t, f.source.CreateCollection(ctx, "metrics",
		options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("ts"))))
	_, err := f.source.Collection("metrics").InsertOne(ctx, bson.M{"ts": time.Now(), "v": 1})
	require.NoError(t, err)
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(78)}})
	require.NoError(t, err)
	before := f.degrade(t, "time-series collection metrics")
	beforeState := f.readIngestState(t, before.BranchID)

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "write-back refused: following ended as degraded")

	for range 2 {
		_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
		require.ErrorContains(t, err, "time-series")
		after := f.status(t)
		require.Equal(t, "degraded", after.State)
		require.Equal(t, before.Error, after.Error)
		require.False(t, after.ResyncUnfinished)
		require.True(t, bytes.Equal(beforeState, f.readIngestState(t, before.BranchID)), "preflight changed the stored ingest state")
	}

	require.NotEqual(t, f.sourceState(t, nil)["catalog"]["item-00"]["stock"], f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "write-back refused: following ended as degraded")
}
