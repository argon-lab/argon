package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFollow_DroppedSourceViewIsIgnored(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
	f.importOnline(t, walcli.OnlineImport{})

	require.NoError(t, f.source.Collection("in_stock").Drop(ctx))
	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(77)}})
	require.NoError(t, err)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.Equal(t, "exact", f.waitCaughtUp(t).Mode)
	require.NoError(t, stop())
	require.Equal(t, "stopped", f.status(t).State)
	require.EqualValues(t, 77, f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	f.requireMainEqualsSource(t, 0, nil, "after the view drop")
}
