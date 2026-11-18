package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFollow_RecreatedSourceViewIsIgnoredAfterRestart(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
	f.importOnline(t, walcli.OnlineImport{})
	require.Equal(t, map[string]bool{"catalog": true}, collectionNames(f.mainState(t, 0)))

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(77)}})
	require.NoError(t, err)
	require.Equal(t, "exact", f.waitCaughtUp(t).Mode)
	require.NoError(t, stop())
	head := f.main(t).HeadLSN

	require.NoError(t, f.source.Collection("in_stock").Drop(ctx))
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(78)}})
	require.NoError(t, err)

	stop = f.follow(t, f.restart(t), walcli.FollowSourceOptions{})
	require.Equal(t, "exact", f.waitCaughtUp(t).Mode)
	require.NoError(t, stop())
	require.Equal(t, "stopped", f.status(t).State)
	require.Equal(t, head+1, f.main(t).HeadLSN, "only the catalog update adds an entry")
	require.EqualValues(t, 78, f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	f.requireMainEqualsSource(t, 0, nil, "after the view is recreated")
}
