package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/argon-lab/argon/v2/pkg/walcli"
)

// Control: a time-series collection or a view that exists before the import
// makes the online import refuse.
func TestOnlineImport_ViewsAreSkippedAndTimeSeriesRefused(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"timeseries", "view"} {
		t.Run(kind, func(t *testing.T) {
			f := newFollowFixture(t, metadataDeploymentURI)
			f.seedSmall(t)
			if kind == "timeseries" {
				require.NoError(t, f.source.CreateCollection(ctx, "metrics",
					options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("ts"))))
			} else {
				require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
			}
			_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: f.sourceURI, Database: f.source.Name(), Project: f.project})
			t.Logf("import with a pre-existing %s: %v", kind, err)
			if kind == "view" {
				// A view holds no documents of its own: it is not imported
				// and does not stand in the way.
				require.NoError(t, err)
				require.Equal(t, map[string]bool{"catalog": true}, collectionNames(f.mainState(t, 0)))
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "import ordinary collections only")
		})
	}
}

// A view created while following changes nothing main could hold, and
// following goes on. A time-series collection created while following holds
// measurements main can never receive: following ends as degraded there
// instead of reporting main as equal to the source.
func TestFollow_ViewIsIgnoredAndTimeSeriesIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(77)}})
	require.NoError(t, err)
	info := f.waitCaughtUp(t)
	require.Equal(t, "exact", info.Mode)
	require.EqualValues(t, 77, f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	require.NoError(t, stop())
	head := f.main(t).HeadLSN

	require.NoError(t, f.source.CreateCollection(ctx, "metrics",
		options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("ts"))))
	_, err = f.source.Collection("metrics").InsertOne(ctx, bson.M{"ts": time.Now(), "v": 1})
	require.NoError(t, err)
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(78)}})
	require.NoError(t, err)
	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
	require.ErrorContains(t, followErr, "time-series collection metrics")
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", status.State)
	require.Equal(t, head, f.main(t).HeadLSN, "nothing after the creation is published")

	// A resync cannot make main equal such a source either, and says so.
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, "time-series")
}
