package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_RefusedWhileResyncIsUnfinished(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	settings := f.source.Collection("settings")
	_, err := settings.InsertOne(ctx, bson.M{"_id": "s1", "v": int32(1)})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(100)}})
	require.NoError(t, err)

	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = f.services.ResyncSource(attempt, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, BatchSize: 5, Force: true,
		Progress: func(p walcli.ResyncProgress) {
			if p.Puts > 0 {
				cancel()
			}
		}})
	require.Error(t, err)
	interrupted := f.status(t)
	require.True(t, interrupted.ResyncUnfinished)
	require.Equal(t, "stopped", interrupted.State)

	// This document still agrees with the source when the comparison stops.
	info, sandboxDB := f.agentSandbox(t, "settings-edit")
	_, err = sandboxDB.Collection("settings").UpdateOne(ctx, bson.M{"_id": "s1"}, bson.M{"$set": bson.M{"v": int32(2)}})
	require.NoError(t, err)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 1)

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "resync of the followed branch has not finished")
	inSource, err := settings.FindOne(ctx, bson.M{"_id": "s1"}).Raw()
	require.NoError(t, err)
	require.EqualValues(t, 1, inSource.Lookup("v").Int32())
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
}
