package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// (a) An update to a document in a capped collection on a source without
// images. Nothing is old: the event is read seconds after it happened.
func TestFollow_CappedCollectionUpdateIsDegraded(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	events := f.cappedCollection(t, "events")
	var err error

	// What the server itself says about the read the follower will make.
	probe := f.source.RunCommand(ctx, bson.D{{Key: "find", Value: "events"}, {Key: "limit", Value: 1},
		{Key: "readConcern", Value: bson.D{{Key: "level", Value: "snapshot"}, {Key: "atClusterTime", Value: f.sourceTime(t)}}}})
	t.Logf("server reply to a snapshot read on the capped collection at the CURRENT cluster time: %v", probe.Err())

	_, importErr := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: f.sourceURI, Database: f.source.Name(), Project: f.project})
	t.Logf("online import with a capped collection present: err=%v", importErr)
	require.NoError(t, importErr)

	head := f.main(t).HeadLSN
	_, err = events.UpdateOne(ctx, bson.M{"_id": "e1"}, bson.M{"$set": bson.M{"n": int32(1)}})
	require.NoError(t, err)
	f.tick(t)

	for attempt := 1; attempt <= 2; attempt++ {
		followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
		info, err := f.services.SourceStatus(ctx, f.project)
		require.NoError(t, err)
		t.Logf("attempt %d (no flags): err=%v", attempt, followErr)
		t.Logf("attempt %d status: state=%s error=%s", attempt, info.State, info.Error)
		require.Error(t, followErr)
		require.Equal(t, "degraded", info.State)
		require.Contains(t, info.Error, "exact post-image unavailable")
		require.Equal(t, head, f.main(t).HeadLSN)
	}

	// The first remedy offered: enable images. The pending event stays unreadable.
	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{EnableSourceImages: true})
	t.Logf("with --enable-source-images: err=%v", followErr)
	require.Error(t, followErr)

	// The second remedy: --allow-inexact.
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AllowInexact: true})
	info := f.waitCaughtUp(t)
	t.Logf("allow-inexact after first update: mode=%s ranges=%d %+v", info.Mode, len(info.InexactRanges), info.InexactRanges)
	require.NoError(t, stop())
}

// (c) on this server version: collMod by --enable-source-images before the
// backlog is read.
func TestFollow_EnablingImagesKeepsTheBacklogReadable(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{})
	catalog := f.source.Collection("catalog")
	for i := 1; i <= 3; i++ {
		_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-05"}, bson.M{"$set": bson.M{"stock": int32(100 + i)}})
		require.NoError(t, err)
	}
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{EnableSourceImages: true})
	info := f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.Equal(t, "exact", info.Mode)
	entries := f.followedEntries(t, result.ConsistentLSN)
	require.Len(t, entries, 3)
	for i, entry := range entries {
		require.Equal(t, "snapshot", entry.Metadata["capture_images"])
		require.EqualValues(t, 101+i, entry.PostImage.Lookup("stock").Int32())
	}
}

// cappedCollection creates a capped collection on the source and skips the
// test when this server serves capped collections at a past cluster time
// (MongoDB 8 does; 7.0 and earlier refuse). The tests that use it are about
// a collection whose updates cannot be read back.
func (f *followFixture) cappedCollection(t *testing.T, name string) *mongo.Collection {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.source.CreateCollection(ctx, name, options.CreateCollection().SetCapped(true).SetSizeInBytes(1<<20)))
	collection := f.source.Collection(name)
	_, err := collection.InsertOne(ctx, bson.M{"_id": "e1", "n": int32(0)})
	require.NoError(t, err)
	probe := f.source.RunCommand(ctx, bson.D{{Key: "find", Value: name}, {Key: "limit", Value: 1},
		{Key: "readConcern", Value: bson.D{{Key: "level", Value: "snapshot"}, {Key: "atClusterTime", Value: f.sourceTime(t)}}}})
	if probe.Err() == nil {
		t.Skip("this MongoDB version serves capped collections at a past cluster time, so their updates are followed exactly")
	}
	return collection
}
