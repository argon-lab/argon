package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestFollow_ExactCheckpointIncludesCappedEvictions(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	ctx := context.Background()
	require.NoError(t, f.source.CreateCollection(ctx, "recent", options.CreateCollection().
		SetCapped(true).SetSizeInBytes(65536).SetMaxDocuments(2)))
	recent := f.source.Collection("recent")
	for _, id := range []string{"first", "second"} {
		_, err := recent.InsertOne(ctx, bson.M{"_id": id})
		require.NoError(t, err)
	}
	f.importOnline(t, walcli.OnlineImport{})
	f.requireMainEqualsSource(t, 0, nil, "after online import")
	done := f.follow(t, f.services, walcli.FollowSourceOptions{})
	for _, id := range []string{"third", "fourth"} {
		_, err := recent.InsertOne(ctx, bson.M{"_id": id})
		require.NoError(t, err)
	}
	status := f.waitCaughtUp(t)
	require.NoError(t, done())
	main := f.mainState(t, status.ConsistentLSN)["recent"]
	source := f.sourceState(t, nil)["recent"]
	require.Len(t, source, 2)
	require.Equal(t, "exact", status.Mode)
	require.NoError(t, status.HealthError())
	require.Equal(t, source, main, "exact checkpoint must reflect the capped collection's evictions")
}
