//go:build unix

package wal_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestFollow_CanceledReplacementLeavesCheckpointUnverified(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	useShortSourceLease(t)

	child := startFollowerProcess(t, f, filepath.Join(t.TempDir(), "owner.json"))
	last := f.status(t)
	require.NoError(t, child.Process.Kill())
	require.ErrorAs(t, child.Wait(), new(*exec.ExitError))
	awaitSourceLeaseExpiration(t, f, last.Owner)

	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	f.tick(t)
	expired := f.status(t)
	require.True(t, expired.LeaseExpired)
	require.ErrorContains(t, expired.HealthError(), "has not been heard from since")

	// Cancel the replacement after it acquires the lease, before MongoDB's
	// change stream opens. The saved source position cannot have advanced.
	replacement, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	watchCalled := false
	err = f.services.FollowSource(replacement, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI,
		Watch: func(ctx context.Context, _ *mongo.Database, _ *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			watchCalled = true
			starting := f.status(t)
			require.Equal(t, "starting", starting.State)
			require.Equal(t, expired.Position, starting.Position)
			cancel()
			return nil, ctx.Err()
		},
	})
	require.True(t, watchCalled)
	require.NoError(t, err)
	stopped := f.status(t)
	mainStock := f.mainState(t, 0)["catalog"]["item-00"]["stock"]
	sourceStock := f.sourceState(t, nil)["catalog"]["item-00"]["stock"]
	require.Equal(t, expired.Position, stopped.Position)
	require.Equal(t, expired.ConsistentLSN, stopped.ConsistentLSN)
	require.Equal(t, int32(10), mainStock)
	require.Equal(t, int32(99), sourceStock)
	require.Nil(t, stopped.Owner)
	require.False(t, stopped.LeaseExpired)
	require.ErrorContains(t, stopped.HealthError(), "last renewed its lease")
	require.ErrorContains(t, stopped.HealthError(), "Start `argon source follow`")
	require.True(t, stopped.Unverified)

	resyncCtx, cancelResync := context.WithTimeout(ctx, 5*time.Second)
	defer cancelResync()
	_, err = f.services.ResyncSource(resyncCtx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, "`argon source follow` can resume from the saved position")
	require.True(t, f.status(t).Unverified)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	verified := f.waitCaughtUp(t)
	require.False(t, verified.Unverified)
	require.NoError(t, verified.HealthError())
	require.Equal(t, int32(99), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	require.NoError(t, stop())
	require.NoError(t, f.status(t).HealthError())
}

func TestResync_RepairsUnverifiedPositionThatCannotBeResumed(t *testing.T) {
	t.Run("invalid saved token", func(t *testing.T) {
		checkResyncRepairsUnverifiedPosition(t, true)
	})
	t.Run("saved token outside the oplog", func(t *testing.T) {
		checkResyncRepairsUnverifiedPosition(t, false)
	})
}

func checkResyncRepairsUnverifiedPosition(t *testing.T, corruptToken bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	last := f.status(t)
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	_, err := state.UpdateByID(ctx, last.BranchID, bson.M{"$set": bson.M{"capture_status.state": "running"}})
	require.NoError(t, err)
	replacement, stop := context.WithCancel(ctx)
	defer stop()
	require.NoError(t, f.services.FollowSource(replacement, f.project, walcli.FollowSourceOptions{
		URI: f.sourceURI,
		Watch: func(ctx context.Context, _ *mongo.Database, _ *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			stop()
			return nil, ctx.Err()
		},
	}))
	require.ErrorContains(t, f.status(t).HealthError(), "left without a lease owner")

	if corruptToken {
		_, err = state.UpdateByID(ctx, last.BranchID, bson.M{"$set": bson.M{"resume_token": bson.M{"_data": "invalid"}}})
		require.NoError(t, err)
	}
	var checkedSavedPosition bool
	info, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI,
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			if opts.ResumeAfter != nil {
				checkedSavedPosition = true
				return nil, mongo.CommandError{Code: 286, Message: "resume position is no longer in the oplog"}
			}
			f.tick(t)
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, !corruptToken, checkedSavedPosition)
	require.NoError(t, f.status(t).HealthError())
}
