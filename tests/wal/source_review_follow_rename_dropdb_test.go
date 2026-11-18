package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// reproDegradedTwice follows twice. Both runs must end degraded with the
// operation named, and main must stay at the change before the event: the
// second run proves the saved position was not moved past it.
func reproDegradedTwice(t *testing.T, f *followFixture, want string, stock int) {
	ctx := context.Background()
	for round := 1; round <= 2; round++ {
		followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
		require.Error(t, followErr, "round %d", round)
		t.Logf("round %d follow error: %v", round, followErr)
		info, err := f.services.SourceStatus(ctx, f.project)
		require.NoError(t, err)
		t.Logf("round %d status: state=%s error=%q", round, info.State, info.Error)
		require.Equal(t, "degraded", info.State, "round %d", round)
		require.Contains(t, info.Error, want, "round %d", round)
		doc, err := f.services.Materializer.MaterializeDocument(f.main(t), "catalog", wal.DocumentIDString("item-00"))
		require.NoError(t, err)
		require.EqualValues(t, stock, doc["stock"], "round %d", round)
	}
}

// followedUpToHere lets the follower store everything so far and stops it.
// The update before the collection-level change is then already in main: on
// MongoDB 6.0 it could not be read back at its own cluster time once the
// collection has been renamed or dropped.
func followedUpToHere(t *testing.T, f *followFixture) {
	t.Helper()
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
}

func TestFollow_SourceCollectionRenameIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})

	catalog := f.source.Collection("catalog")
	_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	followedUpToHere(t, f)
	db := f.source.Name()
	require.NoError(t, f.sourceClient.Database("admin").RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: db + ".catalog"}, {Key: "to", Value: db + ".catalog_v2"}}).Err())
	_, err = f.source.Collection("catalog_v2").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)

	reproDegradedTwice(t, f, "rename of catalog", 1)
}

func TestFollow_SourceDatabaseDropIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})

	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	followedUpToHere(t, f)
	require.NoError(t, f.source.Drop(ctx))
	_, err = f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "item-00", "stock": int32(2)})
	require.NoError(t, err)

	reproDegradedTwice(t, f, "drop", 1)
}

// The existing drop test, with the second follow added.
func TestFollow_SourceCollectionDropStaysDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)
	reproDegradedTwice(t, f, "drop of scratch", 1)
}
