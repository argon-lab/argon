package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Even with automatic resync enabled, a second follower cannot change the
// status of the current owner. A successful checkpoint also clears a stale
// degraded report from a process that does not understand leases.
func TestFollow_HealthyCheckpointClearsStaleDegradedStatus(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	_ = f.follow(t, f.services, walcli.FollowSourceOptions{})
	before := f.waitCaughtUp(t)

	other := f.restart(t)
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resyncs := 0
	err := other.FollowSource(attempt, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI, AutoResync: true, OnResync: func(*walcli.ResyncInfo) { resyncs++ },
	})
	require.ErrorContains(t, err, "follow is already running")
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.Zero(t, resyncs)
	require.Equal(t, before.State, f.status(t).State)

	result, err := f.ingestState().UpdateOne(ctx, bson.M{"_id": before.BranchID}, bson.M{"$set": bson.M{
		"capture_status.state": "degraded", "capture_status.error": "the head moved without this follower",
	}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)

	_, err = f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "after-degraded-report"})
	require.NoError(t, err)
	f.tick(t)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, err := f.services.SourceStatus(ctx, f.project)
		if !assert.NoError(collect, err) || !assert.NotNil(collect, status) {
			return
		}
		assert.Greater(collect, status.HeadLSN, before.HeadLSN)
		assert.Equal(collect, "running", status.State)
		assert.Empty(collect, status.Error)
		assert.False(collect, status.ResyncUnfinished)
	}, 15*time.Second, 100*time.Millisecond)
	f.requireMainEqualsSource(t, 0, nil, "after the healthy checkpoint")
}
