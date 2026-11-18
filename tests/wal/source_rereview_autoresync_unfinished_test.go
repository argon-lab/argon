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
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (f *followFixture) requireUnfinishedResyncCompleted(t *testing.T) {
	t.Helper()
	services := f.restart(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI})
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.ErrorContains(t, err, "has not finished")
	require.ErrorContains(t, err, "argon source resync")
	require.True(t, f.status(t).ResyncUnfinished)

	resyncs := make(chan *walcli.ResyncInfo, 2)
	stop := f.follow(t, services, walcli.FollowSourceOptions{
		AutoResync: true,
		OnResync:   func(info *walcli.ResyncInfo) { resyncs <- info },
	})
	require.Len(t, resyncs, 1)
	resync := <-resyncs
	require.Positive(t, resync.ConsistentLSN)
	require.False(t, f.status(t).ResyncUnfinished)

	_, err = f.source.Collection("catalog").InsertOne(context.Background(), bson.M{"_id": "after-resync"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.Empty(t, resyncs)
	f.requireMainEqualsSource(t, 0, nil, "after restarting with automatic resync")
}

// Restarting with --auto-resync completes a resync interrupted after it
// wrote part of the difference.
func TestFollow_AutoResyncCompletesInterruptedResync(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	scratch := make([]interface{}, 0, 60)
	for i := 0; i < 60; i++ {
		scratch = append(scratch, bson.M{"_id": int32(i)})
	}
	_, err := f.source.Collection("scratch").InsertMany(ctx, scratch)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	for i := 0; i < 20; i++ {
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$set": bson.M{"stock": int32(100 + i)}})
		require.NoError(t, err)
	}
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	for i := 0; i < 20; i++ {
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$set": bson.M{"stock": int32(200 + i)}})
		require.NoError(t, err)
	}
	f.degrade(t, "drop of scratch")

	// The first batch that changes main is enough to leave a mixed state.
	attempt, cancel := context.WithCancel(ctx)
	_, err = f.services.ResyncSource(attempt, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, BatchSize: 5,
		Progress: func(p walcli.ResyncProgress) {
			if p.Puts > 0 {
				cancel()
			}
		}})
	cancel()
	require.Error(t, err)
	require.True(t, f.status(t).ResyncUnfinished)
	f.requireUnfinishedResyncCompleted(t)
}

// Restarting with --auto-resync also completes a resync that was stopped by
// another collection drop during replay.
func TestFollow_AutoResyncCompletesResyncInterruptedByDrop(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	for _, name := range []string{"scratch", "scratch2"} {
		_, err := f.source.Collection(name).InsertOne(ctx, bson.M{"_id": 1})
		require.NoError(t, err)
	}
	f.importOnline(t, walcli.OnlineImport{})
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	f.degrade(t, "drop of scratch")

	dropped := false
	_, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI,
		Progress: func(p walcli.ResyncProgress) {
			if !dropped {
				dropped = true
				require.NoError(t, f.source.Collection("scratch2").Drop(ctx))
			}
		}})
	require.ErrorContains(t, err, "drop of scratch2")
	failed := f.status(t)
	require.Equal(t, "degraded", failed.State)
	require.True(t, failed.ResyncUnfinished)

	f.requireUnfinishedResyncCompleted(t)
}

func TestFollow_AutoResyncSignalsReadyAfterLostPositionAtStartup(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	var watches int
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{
		AutoResync: true,
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			watches++
			if watches == 1 {
				return nil, mongo.CommandError{Code: 286, Name: "ChangeStreamHistoryLost"}
			}
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.NoError(t, stop())
	require.Equal(t, 2, watches)
}
