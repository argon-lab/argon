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

// Pause between server batches once the follower advances the branch. This
// lets the caller inspect a committed prefix and stop before the next read.
func (f *followFixture) stopAfterFirstCheckpoint(t *testing.T, ctx context.Context, head int64, allowInexact bool, inspect func(*walcli.SourceInfo)) {
	t.Helper()
	worker, stop := context.WithCancel(ctx)
	defer stop()
	checkpoint := make(chan *walcli.SourceInfo)
	monitor := &event.CommandMonitor{Started: func(_ context.Context, cmd *event.CommandStartedEvent) {
		if cmd.CommandName != "getMore" {
			return
		}
		info, err := f.services.SourceStatus(worker, f.project)
		if err != nil || info.HeadLSN <= head {
			return
		}
		select {
		case checkpoint <- info:
			<-worker.Done()
		case <-worker.Done():
		}
	}}
	reader, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Disconnect(context.Background())) }()
	finished := make(chan struct{})
	var followErr error
	go func() {
		defer close(finished)
		followErr = f.services.FollowSource(worker, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI, AllowInexact: allowInexact,
			Watch: func(ctx context.Context, db *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				return reader.Database(db.Name()).Watch(ctx, mongo.Pipeline{}, opts.SetBatchSize(1))
			},
		})
	}()
	defer func() {
		stop()
		<-finished
		require.NoError(t, followErr)
	}()
	select {
	case info := <-checkpoint:
		inspect(info)
	case <-finished:
		t.Fatalf("follow ended before its first checkpoint: %v", followErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSource_OpenCatchUpRangeIsUnhealthyAfterStop(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, reproSourceURI(t))
	cli := buildSourceCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.seedSmall(t)
	events := f.cappedCollection(t, "events")
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)
	plan := f.pendingSourceEdit(t, ctx)

	// The latest image is 999 when the first update is replayed, but stock
	// had already changed to 99 before the source ever held that image.
	for i := int32(1); i <= 4; i++ {
		_, err := events.UpdateByID(ctx, "e1", bson.M{"$set": bson.M{"n": i}})
		require.NoError(t, err)
	}
	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	_, err = events.UpdateByID(ctx, "e1", bson.M{"$set": bson.M{"n": int32(999)}})
	require.NoError(t, err)
	f.tick(t)

	f.stopAfterFirstCheckpoint(t, ctx, before.HeadLSN, true, func(open *walcli.SourceInfo) {
		require.Equal(t, "catch_up", open.Mode)
		require.Equal(t, ingest.ReasonCatchUp, open.Health.Reason)
		require.Error(t, open.HealthError())
	})
	stopped, exit := f.sourceStatusCLI(t, ctx, cli)
	require.Equal(t, "stopped", stopped.State)
	require.Equal(t, "catch_up", stopped.Mode)
	require.Equal(t, ingest.ReasonCatchUp, stopped.Health.Reason)
	main, source := f.mainState(t, stopped.HeadLSN), f.sourceState(t, nil)
	require.Equal(t, int32(999), main["events"]["e1"]["n"])
	require.Equal(t, int32(10), main["catalog"]["item-00"]["stock"])
	require.Equal(t, int32(999), source["events"]["e1"]["n"])
	require.Equal(t, int32(99), source["catalog"]["item-00"]["stock"])
	require.Error(t, stopped.HealthError())
	require.Equal(t, 1, exit)
	require.False(t, f.deploymentCaptureHealthy(t, ctx))
	f.requireWriteBackRefused(t, ctx, plan, stopped.Health.Message)

	finish := f.follow(t, f.services, walcli.FollowSourceOptions{AllowInexact: true})
	recovered := f.waitCaughtUp(t)
	require.NoError(t, finish())
	f.requireMainEqualsSource(t, recovered.HeadLSN, nil, "after catch-up completes")
	_, exit = f.sourceStatusCLI(t, ctx, cli)
	require.Zero(t, exit)
	require.True(t, f.deploymentCaptureHealthy(t, ctx))
}
