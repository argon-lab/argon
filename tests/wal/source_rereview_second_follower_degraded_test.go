package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A rolling restart can start the new process before the existing follower
// stops. The new process is refused while the first remains healthy and
// write-back remains available.
func TestFollow_RefusesASecondFollower(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	before := f.waitCaughtUp(t)

	info, sandboxDB := f.agentSandbox(t, "stock-edit")
	_, err := sandboxDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"note": "sold out"}})
	require.NoError(t, err)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 1)

	other := f.restart(t)
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = other.FollowSource(attempt, f.project, walcli.FollowSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, "follow is already running")
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.NotNil(t, before.Owner)
	require.Equal(t, "follow", before.Owner.Kind)
	require.NotEmpty(t, before.Owner.Host)
	require.Positive(t, before.Owner.PID)
	require.False(t, before.Owner.HeartbeatAt.IsZero())
	require.ErrorContains(t, err, before.Owner.Host)
	require.ErrorContains(t, err, fmt.Sprintf("pid %d", before.Owner.PID))
	require.ErrorContains(t, err, "since")
	after := f.status(t)
	require.NotNil(t, after.Owner)
	require.Equal(t, before.Owner.StartedAt, after.Owner.StartedAt)
	require.Equal(t, before.State, after.State)
	require.Empty(t, after.Error)

	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Applied)
	caught := f.waitCaughtUp(t)
	require.Greater(t, caught.HeadLSN, before.HeadLSN)
	f.requireMainEqualsSource(t, 0, nil, "after the first follower caught up")
	require.NoError(t, stop())
	require.Nil(t, f.status(t).Owner)
}

func TestResync_RefusesALiveFollower(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	_ = f.follow(t, f.services, walcli.FollowSourceOptions{})
	before := f.waitCaughtUp(t)
	require.NotNil(t, before.Owner)
	other := f.restart(t)

	for _, force := range []bool{true, false} {
		_, err := other.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, Force: force})
		require.ErrorContains(t, err, "follow is already running")
		var configuration *ingest.ConfigurationError
		require.ErrorAs(t, err, &configuration)
	}
	after := f.status(t)
	require.Equal(t, "running", after.State)
	require.Empty(t, after.Error)
	require.NotNil(t, after.Owner)
	require.Equal(t, before.Owner.StartedAt, after.Owner.StartedAt)
	require.False(t, after.ResyncUnfinished)

	_, err := f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "after-refusal"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	f.requireMainEqualsSource(t, 0, nil, "after resync was refused")
}
