package wal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func requireSourcePositionDigest(t *testing.T, checkpoint bson.Raw, field string) {
	t.Helper()
	position, err := checkpoint.LookupErr(field)
	require.NoError(t, err)
	want := sha256.Sum256(position.Value)
	subtype, got, ok := checkpoint.Lookup("capture_status", "source", "position_digest").BinaryOK()
	require.True(t, ok)
	require.Zero(t, subtype)
	require.Equal(t, want[:], got)
}

func TestSource_ReplacedResumeTokenAtSameClusterTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	require.NoError(t, f.source.CreateCollection(ctx, "scratch"))
	f.importOnline(t, walcli.OnlineImport{})
	catalog := f.source.Collection("catalog")
	stream, err := catalog.Watch(ctx, mongo.Pipeline{}, options.ChangeStream().SetShowExpandedEvents(true))
	require.NoError(t, err)
	defer func() { require.NoError(t, stream.Close(context.Background())) }()
	session, err := f.sourceClient.StartSession()
	require.NoError(t, err)
	defer session.EndSession(context.Background())
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		for _, id := range []string{"item-00", "item-01"} {
			if _, err := catalog.UpdateByID(sc, id, bson.M{"$inc": bson.M{"stock": int32(1)}}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	require.NoError(t, err)

	var tokens [2]bson.Raw
	var times [2]bson.RawValue
	for i := range tokens {
		require.True(t, stream.Next(ctx), "missing transaction event: %v", stream.Err())
		token, ok := stream.Current.Lookup("_id").DocumentOK()
		require.True(t, ok)
		tokens[i] = bytes.Clone(token)
		times[i] = stream.Current.Lookup("clusterTime")
		times[i].Value = bytes.Clone(times[i].Value)
	}
	require.Equal(t, times[0], times[1])
	require.NotEqual(t, tokens[0], tokens[1])

	// The drop ends replay immediately after the transaction. Its second
	// event remains the saved token even if the source clock advances.
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	before := f.degrade(t, "drop")
	filter := bson.M{"_id": before.BranchID}
	checkpoint, err := f.ingestState().FindOne(ctx, filter).Raw()
	require.NoError(t, err)
	require.Equal(t, tokens[1], checkpoint.Lookup("resume_token").Document())
	require.Equal(t, times[1], checkpoint.Lookup("capture_status", "source", "position"))
	_, err = f.ingestState().UpdateOne(ctx, filter, bson.M{"$set": bson.M{"resume_token": tokens[0]}})
	require.NoError(t, err)
	broken := f.status(t)
	require.Equal(t, ingest.ReasonPositionMissing, broken.Health.Reason)
	require.Equal(t, before.Position, broken.Position)
	err = f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, broken.Health.Message)

	var boundary bson.Raw
	var readErr error
	result, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI, Progress: func(walcli.ResyncProgress) {
			if boundary == nil && readErr == nil {
				boundary, readErr = f.ingestState().FindOne(ctx, filter).Raw()
			}
		},
	})
	require.NoError(t, err)
	require.NoError(t, readErr)
	require.Zero(t, boundary.Lookup("resume_token").Type)
	requireSourcePositionDigest(t, boundary, "start_at")
	after := f.status(t)
	require.Equal(t, ingest.ReasonExact, after.Health.Reason)
	require.Equal(t, result.ConsistentLSN, after.HeadLSN)
	repaired, err := f.ingestState().FindOne(ctx, filter).Raw()
	require.NoError(t, err)
	requireSourcePositionDigest(t, repaired, "resume_token")
	f.requireMainEqualsSource(t, after.HeadLSN, nil, "after replacing the saved position")
}

func TestFollow_CheckpointWithoutDigestGainsOne(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	filter := bson.M{"_id": f.main(t).ID}
	imported, err := f.ingestState().FindOne(ctx, filter).Raw()
	require.NoError(t, err)
	requireSourcePositionDigest(t, imported, "resume_token")
	_, err = f.ingestState().UpdateOne(ctx, filter, bson.M{"$unset": bson.M{"capture_status.source.position_digest": ""}})
	require.NoError(t, err)
	require.Equal(t, ingest.ReasonExact, f.status(t).Health.Reason)

	_, err = f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$inc": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	updated, err := f.ingestState().FindOne(ctx, filter).Raw()
	require.NoError(t, err)
	requireSourcePositionDigest(t, updated, "resume_token")
	require.Equal(t, ingest.ReasonExact, f.status(t).Health.Reason)
}
