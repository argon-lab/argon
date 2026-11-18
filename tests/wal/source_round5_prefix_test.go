package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFollow_ReplacementVerifiesOnlyAfterOpeningBoundary(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := f.status(t)
	// A lost owner leaves its last checkpoint for the replacement.
	_, err := f.ingestState().UpdateByID(ctx, before.BranchID, bson.M{"$set": bson.M{"capture_status.state": "running"}})
	require.NoError(t, err)
	for _, stock := range []int32{11, 12, 13, 14} {
		_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": stock}})
		require.NoError(t, err)
	}
	minimumBoundary := f.sourceTime(t)
	checkPrefix := func(info *walcli.SourceInfo) {
		t.Helper()
		require.NotNil(t, info.Position)
		at := primitive.Timestamp{T: info.Position.T, I: info.Position.I}
		require.True(t, at.Before(minimumBoundary))
		require.Equal(t, "exact", info.Mode)
		require.Equal(t, info.HeadLSN, info.ConsistentLSN)
		require.True(t, info.Unverified)
		require.Error(t, info.HealthError())
		require.False(t, f.deploymentCaptureHealthy(t, ctx))
		stored, err := f.services.Ingest.SourceStatusFor(ctx, info.BranchID)
		require.NoError(t, err)
		require.ErrorContains(t, stored.Health.WriteBackAdmission(), "write-back refused")
	}
	var prefix *walcli.SourceInfo
	f.stopAfterFirstCheckpoint(t, ctx, before.HeadLSN, false, func(info *walcli.SourceInfo) {
		prefix = info
		require.Equal(t, int32(11), f.mainState(t, info.HeadLSN)["catalog"]["item-00"]["stock"])
		checkPrefix(info)
	})
	stopped := f.status(t)
	require.Nil(t, stopped.Owner)
	require.Equal(t, prefix.Position, stopped.Position)
	checkPrefix(stopped)
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	recovered := f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.False(t, recovered.Unverified)
	require.NoError(t, recovered.HealthError())
	require.True(t, f.deploymentCaptureHealthy(t, ctx))
	stored, err := f.services.Ingest.SourceStatusFor(ctx, recovered.BranchID)
	require.NoError(t, err)
	require.NoError(t, stored.Health.WriteBackAdmission())
	f.requireMainEqualsSource(t, recovered.HeadLSN, nil, "after the replacement reached its boundary")
}
