package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// inspectFollowerOpen runs the check with the lease held, then opens the real
// stream and stops the follower as soon as it signals readiness.
func (f *followFixture) inspectFollowerOpen(t *testing.T, check func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := make(chan struct{})
	go func() {
		select {
		case <-ready:
			cancel()
		case <-ctx.Done():
		}
	}()
	opened := false
	err := f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI, Ready: ready,
		Watch: func(ctx context.Context, db *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			select {
			case <-ready:
				t.Fatal("follower signaled readiness before opening the stream")
			default:
			}
			check(ctx)
			opened = true
			return db.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.NoError(t, err)
	require.True(t, opened)
	select {
	case <-ready:
	default:
		t.Fatal("follower never signaled readiness")
	}
}

func TestSource_HealthyCheckpointWhileFollowerRestarts(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.NoError(t, stop())
	before := f.status(t)
	require.Equal(t, "stopped", before.State)
	require.Equal(t, ingest.ReasonExact, before.Health.Reason)

	f.inspectFollowerOpen(t, func(ctx context.Context) {
		during := f.status(t)
		require.Equal(t, "starting", during.State)
		require.NotNil(t, during.Owner)
		require.False(t, during.LeaseExpired)
		require.Equal(t, before.ConsistentTime, during.ConsistentTime)
		require.Equal(t, before.HeadLSN, during.HeadLSN)
		require.Equal(t, during.HeadLSN, during.ConsistentLSN)
		f.requireMainEqualsSource(t, during.HeadLSN, nil, "while reopening the stream")
		require.NoError(t, during.HealthError())
		require.Equal(t, "restarting", string(during.Health.Reason))
		require.True(t, f.deploymentCaptureHealthy(t, ctx))
	})
	require.Equal(t, ingest.ReasonExact, f.status(t).Health.Reason)
}

func TestFollow_StoredCaptureErrorWhileOpening(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	const message = "unrecognized change-stream option"
	err := f.followUntilItStops(t, walcli.FollowSourceOptions{
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 40415, Message: message}
		},
	})
	require.ErrorContains(t, err, message)
	f.inspectFollowerOpen(t, func(ctx context.Context) {
		during := f.status(t)
		require.Equal(t, "starting", during.State)
		require.Equal(t, ingest.ReasonCaptureError, during.Health.Reason)
		require.ErrorContains(t, during.HealthError(), message)
		require.False(t, f.deploymentCaptureHealthy(t, ctx))
	})
	require.Equal(t, ingest.ReasonExact, f.status(t).Health.Reason)
}

func TestWriteBack_WaitsForRestartingFollower(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	plan := f.pendingSourceEdit(t, context.Background())
	f.inspectFollowerOpen(t, func(ctx context.Context) {
		f.requireWriteBackRefused(t, ctx, plan, "wait for the follower to be ready")
	})
}
