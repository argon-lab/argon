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
)

// A second resync cannot replace the replay boundary of a resync already
// comparing documents. The first must still capture a write made after
// its compare cursor read the affected document.
func TestResync_RefusesConcurrentResync(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	interval := ingest.SourceHeartbeatInterval
	ingest.SourceHeartbeatInterval = 200 * time.Millisecond
	t.Cleanup(func() { ingest.SourceHeartbeatInterval = interval })
	checkResyncReplay(t, func(t *testing.T, f *followFixture) {
		owner := f.status(t).Owner
		require.NotNil(t, owner)
		require.Equal(t, "resync", owner.Kind)
		require.Eventually(t, func() bool {
			info, err := f.services.SourceStatus(context.Background(), f.project)
			return err == nil && info.Owner != nil && info.Owner.HeartbeatAt.After(owner.HeartbeatAt)
		}, 5*time.Second, 50*time.Millisecond)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		other := f.restart(t)
		before := f.readIngestState(t, f.main(t).ID)

		_, err := other.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
		require.ErrorContains(t, err, "resync is already running")
		var configuration *ingest.ConfigurationError
		require.ErrorAs(t, err, &configuration)
		automaticResyncs := 0
		err = other.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI, AutoResync: true, OnResync: func(*walcli.ResyncInfo) { automaticResyncs++ },
		})
		require.ErrorContains(t, err, "resync is already running")
		require.ErrorAs(t, err, &configuration)
		require.Zero(t, automaticResyncs)

		after := f.readIngestState(t, f.main(t).ID)
		require.Equal(t, before.Lookup("start_at"), after.Lookup("start_at"))
		require.Equal(t, before.Lookup("resume_token"), after.Lookup("resume_token"))
		require.Equal(t, before.Lookup("capture_status", "state"), after.Lookup("capture_status", "state"))
		require.Equal(t, before.Lookup("capture_status", "error"), after.Lookup("capture_status", "error"))
	})
}

// Even a direct change to the saved position cannot redirect the active
// resync's replay past a source write its comparison did not see.
func TestResync_ReplaysFromItsOwnBoundary(t *testing.T) {
	checkResyncReplay(t, func(t *testing.T, f *followFixture) {
		branchID := f.main(t).ID
		startT, startI := f.readIngestState(t, branchID).Lookup("start_at").Timestamp()
		start := primitive.Timestamp{T: startT, I: startI}
		later := f.sourceTime(t)
		if later.I++; later.I == 0 {
			later.T++
		}
		require.True(t, later.After(start))
		result, err := f.ingestState().UpdateOne(context.Background(), bson.M{"_id": branchID}, bson.M{"$set": bson.M{"start_at": later}})
		require.NoError(t, err)
		require.EqualValues(t, 1, result.ModifiedCount)
	})
}

func checkResyncReplay(t *testing.T, afterCompareRead func(*testing.T, *followFixture)) {
	t.Helper()
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": int32(1)})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	before := f.degrade(t, "drop of scratch")

	changed := false
	result := f.resync(t, f.services, walcli.ResyncSourceOptions{BatchSize: 5,
		Progress: func(p walcli.ResyncProgress) {
			if changed {
				return
			}
			changed = true
			// One server batch held all 20 catalog documents before
			// this update, so the comparison will see the old value.
			require.Equal(t, "compare", p.Phase)
			require.Equal(t, "catalog", p.Collection)
			_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-15"}, bson.M{"$set": bson.M{"stock": int32(999)}})
			require.NoError(t, err)
			require.True(t, f.status(t).ResyncUnfinished)
			afterCompareRead(t, f)
		}})
	require.True(t, changed)
	after := f.requireResynced(t, before, result)
	require.Nil(t, after.Owner, "a finished resync releases its lease")
	require.Equal(t, int32(999), f.mainState(t, result.ConsistentLSN)["catalog"]["item-15"]["stock"])
}
