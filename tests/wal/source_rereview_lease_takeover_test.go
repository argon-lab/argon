package wal_test

import (
	"context"
	"sync"
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

func TestFollow_LeaseTakeoverFencesAnOldCommit(t *testing.T) {
	leasePeriod := ingest.SourceLeasePeriod
	heartbeatInterval := ingest.SourceHeartbeatInterval
	ingest.SourceLeasePeriod = 2 * time.Second
	// Neither process renews during the handoff, simulating a paused owner.
	ingest.SourceHeartbeatInterval = time.Minute
	t.Cleanup(func() {
		ingest.SourceLeasePeriod = leasePeriod
		ingest.SourceHeartbeatInterval = heartbeatInterval
	})

	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	other := f.restart(t)
	branch := f.main(t)

	reading, read := make(chan struct{}), make(chan struct{})
	var once sync.Once
	monitor := &event.CommandMonitor{Started: func(ctx context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" && e.DatabaseName == f.source.Name() {
			once.Do(func() { close(reading) })
			select {
			case <-read:
			case <-ctx.Done():
			}
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(ctx) })

	ready := make(chan struct{})
	oldRun := startSourceRun(t, func(ctx context.Context) error {
		return f.services.Ingest.Follow(ctx, branch.ID, client.Database(f.source.Name()), ingest.FollowOptions{OnReady: func() { close(ready) }})
	})
	awaitSourceRun(t, ready)
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(75)}})
	require.NoError(t, err)
	awaitSourceRun(t, reading)
	f.tick(t)

	owner := func() *ingest.SourceOwner {
		status, err := f.services.Ingest.SourceStatusFor(ctx, branch.ID)
		require.NoError(t, err)
		require.NotNil(t, status)
		require.NotNil(t, status.Owner)
		return status.Owner
	}
	oldOwner := owner()
	expires := oldOwner.HeartbeatAt.Add(ingest.SourceLeasePeriod)
	require.Eventually(t, func() bool {
		var reply struct {
			LocalTime time.Time `bson:"localTime"`
		}
		err := f.services.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&reply)
		return err == nil && reply.LocalTime.After(expires)
	}, 10*time.Second, 25*time.Millisecond)

	opening, open, newReady := make(chan struct{}), make(chan struct{}), make(chan struct{})
	_ = startSourceRun(t, func(ctx context.Context) error {
		return other.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI, Ready: newReady,
			Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				close(opening)
				select {
				case <-open:
					return src.Watch(ctx, mongo.Pipeline{}, opts)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		})
	})
	awaitSourceRun(t, opening)
	newOwner := owner()
	require.NotEqual(t, oldOwner.ID, newOwner.ID)
	require.Equal(t, "follow", newOwner.Kind)
	state := f.readIngestState(t, branch.ID)

	// The successor has acquired the lease but has made no head change.
	// The old commit must fail on ownership even though its head still matches.
	close(read)
	oldErr := oldRun()
	require.Equal(t, branch.HeadLSN, f.main(t).HeadLSN)
	require.Equal(t, state, f.readIngestState(t, branch.ID))
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, oldErr, &configuration)
	require.ErrorContains(t, oldErr, "another process took over")
	entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": branch.ID, "lsn": bson.M{"$gt": branch.HeadLSN}})
	require.NoError(t, err)
	require.Empty(t, entries)

	close(open)
	awaitSourceRun(t, newReady)
	f.waitCaughtUp(t)
	require.Len(t, f.followedEntries(t, branch.HeadLSN), 1)
	f.requireMainEqualsSource(t, 0, nil, "after the new owner caught up")
}
