package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestResync_AfterCollectionIsReplacedByAView(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		imported := f.importOnline(t, walcli.OnlineImport{})
		require.Len(t, f.mainState(t, imported.ConsistentLSN)["catalog"], 20)

		name := f.source.Name()
		require.NoError(t, f.sourceClient.Database("admin").RunCommand(ctx, bson.D{
			{Key: "renameCollection", Value: name + ".catalog"}, {Key: "to", Value: name + ".products"},
		}).Err())
		require.NoError(t, f.source.CreateView(ctx, "catalog", "products", bson.A{}))
		count, err := f.source.Collection("catalog").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		require.EqualValues(t, 20, count, "the view exposes the same documents under the old name")
		before := f.degrade(t, "rename of catalog")

		result := f.resync(t, f.services, walcli.ResyncSourceOptions{BatchSize: 5})
		require.NotContains(t, f.mainState(t, result.ConsistentLSN), "catalog")
		require.Equal(t, []string{"catalog"}, result.RemovedCollections)
		require.EqualValues(t, 20, result.Puts)
		require.EqualValues(t, 20, result.Deletes)
		f.requireResynced(t, before, result)
	})
}
