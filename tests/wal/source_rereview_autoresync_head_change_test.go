package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestFollow_AutoResyncStopsWhenAnotherWriterMovesHead(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)
	branches := f.services.Client.Database(f.metadataName).Collection("wal_branches")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	resyncs := 0
	err := f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI, AutoResync: true,
		OnResync: func(*walcli.ResyncInfo) {
			resyncs++
			cancel()
		},
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			// The follower has read the old head. A writer without the lease
			// advances it before the follower can publish the next change.
			result, err := branches.UpdateOne(ctx, bson.M{"_id": before.BranchID, "head_lsn": before.HeadLSN},
				bson.M{"$inc": bson.M{"head_lsn": 1, "mutation_version": 1}})
			require.NoError(t, err)
			require.EqualValues(t, 1, result.MatchedCount)
			_, err = f.source.Collection("catalog").DeleteOne(ctx, bson.M{"_id": "item-00"})
			require.NoError(t, err)
			f.tick(t)
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.Zero(t, resyncs)
	var gap *ingest.CaptureError
	require.ErrorAs(t, err, &gap)
	require.ErrorContains(t, err, "moved without")
	after := f.status(t)
	require.Equal(t, "degraded", after.State)
	require.False(t, after.ResyncUnfinished)
	require.Equal(t, before.HeadLSN+1, after.HeadLSN)
	require.Equal(t, before.Position, after.Position)
	require.Equal(t, before.InexactRanges, after.InexactRanges)
}
