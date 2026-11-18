package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSource_MissingVerifiedCheckpointRequiresResync(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)
	require.Equal(t, ingest.ReasonExact, before.Health.Reason)
	plan := f.pendingSourceEdit(t, ctx)
	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)

	result, err := f.ingestState().UpdateByID(ctx, before.BranchID, bson.M{"$unset": bson.M{"capture_status.source.consistent": ""}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)
	broken := f.status(t)
	require.Equal(t, before.HeadLSN, broken.HeadLSN)
	require.Equal(t, before.Position, broken.Position)
	require.Equal(t, "exact", broken.Mode)
	require.Equal(t, ingest.ReasonCheckpointUnverified, broken.Health.Reason)
	require.False(t, f.deploymentCaptureHealthy(t, ctx))

	opened := false
	err = f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI,
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			opened = true
			return nil, mongo.CommandError{Code: 40415, Message: "unexpected stream open"}
		},
	})
	require.False(t, opened)
	require.ErrorContains(t, err, broken.Health.Message)
	f.requireWriteBackRefused(t, ctx, plan, broken.Health.Message)

	repaired, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.NoError(t, err)
	after := f.status(t)
	require.Equal(t, ingest.ReasonExact, after.Health.Reason)
	require.Equal(t, repaired.ConsistentLSN, after.HeadLSN)
	f.requireMainEqualsSource(t, after.HeadLSN, nil, "after resync restores the verified checkpoint")
}

func TestSource_ReplacedResumeTokenCannotSkipAChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)
	require.Equal(t, ingest.ReasonExact, before.Health.Reason)

	// This is a genuine token from the same collection, but resuming from it
	// would skip this write, which the branch has never captured.
	catalog := f.source.Collection("catalog")
	stream, err := catalog.Watch(ctx, mongo.Pipeline{})
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close(context.Background())) }()
	f.tick(t) // Move past even an idle import's one-tick-ahead opening token.
	_, err = catalog.UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	require.True(t, stream.Next(ctx), "expected the catalog update: %v", stream.Err())
	result, err := f.ingestState().UpdateByID(ctx, before.BranchID, bson.M{"$set": bson.M{"resume_token": stream.ResumeToken()}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)
	after := f.status(t)
	require.Equal(t, before.HeadLSN, after.HeadLSN)
	require.Equal(t, before.Position, after.Position)
	require.Equal(t, ingest.ReasonPositionMissing, after.Health.Reason)
	require.False(t, f.deploymentCaptureHealthy(t, ctx))
}

func TestOnlineImport_ReplayRequiresAnUnpublishedProject(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)
	point, err := f.services.Ingest.CatchUp(context.Background(), before.BranchID, f.source,
		primitive.Timestamp{T: before.Position.T, I: before.Position.I}, before.HeadLSN, 1, "online import", nil)
	require.Nil(t, point)
	require.ErrorContains(t, err, "requires an unpublished project")
	require.Equal(t, before, f.status(t))
}
