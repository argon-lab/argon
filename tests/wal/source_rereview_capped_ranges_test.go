package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFollow_CappedCollectionRangesPreserveOtherReasons(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	events := f.cappedCollection(t, "events")
	catalog := f.source.Collection("catalog")
	func() {
		traffic := f.startTraffic(t, 2, 2*time.Millisecond)
		defer traffic.halt()
		f.importOnline(t, walcli.OnlineImport{})
	}()
	beforeResync := f.status(t)
	require.Len(t, beforeResync.InexactRanges, 1)
	require.Contains(t, beforeResync.InexactRanges[0].Reason, "online import")

	_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$inc": bson.M{"stock": 1}})
	require.NoError(t, err)
	f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AllowInexact: true})
	first := f.waitCaughtUp(t)
	require.Len(t, first.InexactRanges, 2)
	require.Equal(t, beforeResync.InexactRanges[0], first.InexactRanges[0])
	require.Contains(t, first.InexactRanges[1].Reason, "resync")

	for i := 1; i <= 3; i++ {
		_, err := events.UpdateOne(ctx, bson.M{"_id": "e1"}, bson.M{"$inc": bson.M{"n": 1}})
		require.NoError(t, err)
		info := f.waitCaughtUp(t)
		require.Len(t, info.InexactRanges, 3)
		last := info.InexactRanges[2]
		require.Contains(t, last.Reason, "updates to events")
		require.Equal(t, first.HeadLSN+1, last.FromLSN, "adjacent capped updates share a range")
		require.Equal(t, info.HeadLSN, last.ToLSN)
	}

	const updates, maxRanges = 34, 32
	var cappedLSNs []int64
	var info *walcli.SourceInfo
	for i := 0; i < updates; i++ {
		// An exact catalog write separates each new capped update.
		_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$inc": bson.M{"stock": 1}})
		require.NoError(t, err)
		exact := f.waitCaughtUp(t)
		_, err = events.UpdateOne(ctx, bson.M{"_id": "e1"}, bson.M{"$inc": bson.M{"n": 1}})
		require.NoError(t, err)
		info = f.waitCaughtUp(t)
		require.Equal(t, exact.HeadLSN+1, info.HeadLSN)
		cappedLSNs = append(cappedLSNs, info.HeadLSN)
	}
	require.NoError(t, stop())
	require.Len(t, info.InexactRanges, maxRanges)
	require.Equal(t, first.InexactRanges, info.InexactRanges[:2], "the import and resync ranges stay listed")
	require.Equal(t, 3+updates-maxRanges, info.InexactRangesDropped)
	for i, span := range info.InexactRanges[2:] {
		lsn := cappedLSNs[updates-(maxRanges-2)+i]
		require.Contains(t, span.Reason, "updates to events")
		require.Equal(t, lsn, span.FromLSN)
		require.Equal(t, lsn, span.ToLSN, "the exact catalog writes stay out of the inexact ranges")
	}
}
