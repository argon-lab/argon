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

func TestFollow_ImagesAreNotEnabledDuringAnUnfinishedResync(t *testing.T) {
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
	// These the follower never sees: it ends at the drop.
	for i := 0; i < 20; i++ {
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$set": bson.M{"stock": int32(200 + i)}})
		require.NoError(t, err)
	}
	f.degrade(t, "drop of scratch")

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
	require.False(t, reproImagesEnabledOn(t, f, "catalog"), "nothing has asked for images yet")

	bounded, cancelFollow := context.WithTimeout(ctx, 60*time.Second)
	defer cancelFollow()
	err = f.services.FollowSource(bounded, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, EnableSourceImages: true})
	require.ErrorIs(t, err, ingest.ErrUnfinishedResync)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	require.ErrorContains(t, err, "The source's collections were not altered")
	require.True(t, f.status(t).ResyncUnfinished)

	var specs []*mongo.CollectionSpecification
	var listErr error
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AutoResync: true, EnableSourceImages: true,
		OnResync: func(*walcli.ResyncInfo) {
			specs, listErr = f.source.ListCollectionSpecifications(ctx, bson.M{"name": "catalog"})
		},
	})
	require.NoError(t, listErr)
	require.Len(t, specs, 1)
	enabled, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
	require.False(t, enabled)
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
	require.NoError(t, stop())
}

func TestFollow_ImagesAreNotEnabledWhileAStoredGapNeedsResync(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": int32(1)})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	before := f.degrade(t, "drop of scratch")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	bounded, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	err = f.services.FollowSource(bounded, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, EnableSourceImages: true})
	require.ErrorContains(t, err, "drop of scratch")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	require.ErrorContains(t, err, "The source's collections were not altered")
	after := f.status(t)
	require.Equal(t, "degraded", after.State)
	require.Equal(t, before.Error, after.Error)
}

func TestFollow_ImagesAreNotEnabledWithoutSavedSourceState(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	result, err := state.UpdateByID(ctx, f.main(t).ID, bson.M{"$unset": bson.M{"capture_status.source": ""}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)

	err = f.followUntilItStops(t, walcli.FollowSourceOptions{EnableSourceImages: true})
	require.ErrorContains(t, err, "the saved source position")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	require.ErrorContains(t, err, "The source's collections were not altered")
}

func TestFollow_ImagesAreNotEnabledIfStreamCannotOpen(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})

	err := f.followUntilItStops(t, walcli.FollowSourceOptions{EnableSourceImages: true,
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 286, Name: "ChangeStreamHistoryLost"}
		},
	})
	require.ErrorContains(t, err, "ChangeStreamHistoryLost")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	require.ErrorContains(t, err, "The source's collections were not altered")
}

func TestFollow_ReportsSourceCollectionsAlteredBeforeFailure(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	err = f.followUntilItStops(t, walcli.FollowSourceOptions{EnableSourceImages: true,
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			// The saved position and stream are valid at startup. Drop a
			// collection as the stream opens so the first run hits the gap.
			if err := src.Collection("scratch").Drop(ctx); err != nil {
				return nil, err
			}
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.ErrorContains(t, err, "drop of scratch")
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
	require.ErrorContains(t, err, "changeStreamPreAndPostImages was enabled on source collection(s)")
	require.ErrorContains(t, err, "catalog")
}
