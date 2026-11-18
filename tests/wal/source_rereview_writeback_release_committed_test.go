package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A collection drop precedes an unrecorded write-back commit in the stream.
// Resync brings the changes into main and leaves the claim intact.
func TestWriteBack_ClaimSurvivesResyncAndFollowing(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	reviewed := f.sourceState(t, nil)

	// Nothing is following. The application drops one of its collections;
	// the stopped follower has not seen that, so write-back is allowed.
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	written := f.sourceState(t, nil)
	require.NotEqual(t, reviewed["catalog"], written["catalog"], "the write-back must have changed production")

	// Restore the claim as it would appear after a crash before the plan update.
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	_, err = plans.UpdateOne(ctx, bson.M{"_id": plan.ID}, bson.M{
		"$set":   bson.M{"status": merge.StatusWritingBack, "write_back.started_at": time.Now().Add(-time.Hour)},
		"$unset": bson.M{"applied_at": "", "strategy": "", "write_back.committed_at": ""},
	})
	require.NoError(t, err)

	// Following ends at the drop, which is before the write-back in the oplog.
	f.degrade(t, "drop of scratch")
	require.Equal(t, merge.StatusWritingBack, f.plan(t, plan).Status)

	f.resync(t, f.services, walcli.ResyncSourceOptions{})
	afterResync := f.plan(t, plan)
	require.Equal(t, merge.StatusWritingBack, afterResync.Status)
	require.NotNil(t, afterResync.WriteBack)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	after := f.plan(t, plan)
	require.Equal(t, written, f.sourceState(t, nil), "production still holds the plan's changes")
	f.requireMainEqualsSource(t, 0, nil, "after resync")
	require.Equal(t, merge.StatusWritingBack, after.Status)
	require.Equal(t, afterResync.WriteBack, after.WriteBack)
	require.Empty(t, after.WriteBackLastError)

	fresh := f.preview(t, info.BranchID)
	require.NotEqual(t, plan.ID, fresh.ID)
	require.Empty(t, fresh.Changes)
	require.Empty(t, fresh.Conflicts)
	result, err := f.services.WriteBackMerge(ctx, fresh.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Zero(t, result.Applied)
	require.Equal(t, written, f.sourceState(t, nil))
}

// An old claim can still belong to a writer whose transaction is in progress.
func TestWriteBack_FollowingKeepsPausedWritersClaim(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	reviewed := f.sourceState(t, nil)

	// An application transaction holds item-01, which the plan updates. The
	// write-back's transaction conflicts with it and the driver retries.
	blocker, err := f.sourceClient.StartSession()
	require.NoError(t, err)
	defer blocker.EndSession(ctx)
	require.NoError(t, blocker.StartTransaction())
	require.NoError(t, mongo.WithSession(ctx, blocker, func(sc mongo.SessionContext) error {
		_, err := f.source.Collection("catalog").UpdateOne(sc, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"held": true}})
		return err
	}))

	type outcome struct {
		result *merge.ApplyResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		done <- outcome{result, err}
	}()

	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	require.Eventually(t, func() bool { return f.plan(t, plan).Status == merge.StatusWritingBack }, 30*time.Second, 50*time.Millisecond)
	aged, err := plans.UpdateOne(ctx, bson.M{"_id": plan.ID, "status": merge.StatusWritingBack},
		bson.M{"$set": bson.M{"write_back.started_at": time.Now().Add(-time.Hour)}})
	require.NoError(t, err)
	require.EqualValues(t, 1, aged.MatchedCount)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.Equal(t, merge.StatusWritingBack, f.plan(t, plan).Status)
	select {
	case o := <-done:
		t.Fatalf("the write-back ended while it was supposed to be held up: %+v %v", o.result, o.err)
	default:
	}

	require.NoError(t, blocker.AbortTransaction(ctx))
	select {
	case o := <-done:
		require.NoError(t, o.err)
		require.Equal(t, 4, o.result.Applied)
	case <-time.After(30 * time.Second):
		t.Fatal("write-back did not finish after the blocking transaction ended")
	}
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	after := f.plan(t, plan)
	written := f.sourceState(t, nil)
	require.NotEqual(t, reviewed["catalog"], written["catalog"], "the write-back committed on production")
	require.Equal(t, merge.StatusApplied, after.Status)
	require.NotNil(t, after.WriteBack.CommittedAt)
	require.NotNil(t, after.WriteBack.SettledAt)
}

func TestWriteBack_CommitReportsPlanRecordMismatch(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")

	// Change the metadata after MongoDB acknowledges the source commit and
	// before the writer records the outcome.
	result, err := writeBackAfterSourceCommit(t, f, plan, func() {
		changed, err := plans.UpdateOne(ctx, bson.M{"_id": plan.ID, "status": merge.StatusWritingBack},
			bson.M{"$set": bson.M{"status": merge.StatusPending}, "$unset": bson.M{"write_back": ""}})
		require.NoError(t, err)
		require.EqualValues(t, 1, changed.MatchedCount)
	})
	require.ErrorContains(t, err, "the source transaction committed")
	require.ErrorContains(t, err, plan.ID.Hex())
	require.ErrorContains(t, err, "could not be marked applied")
	require.Equal(t, 4, result.Applied)
	require.NoError(t, f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-new"}).Err())
	after := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, after.Status)
	require.Nil(t, after.WriteBack)
}

func TestWriteBack_WriterRecordsCommitAfterFollowingSettles(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	var settled *merge.Plan
	result, err := writeBackAfterSourceCommit(t, f, plan, func() {
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		settled = f.plan(t, plan)
		require.Equal(t, merge.StatusApplied, settled.Status)
		require.NotNil(t, settled.WriteBack.SettledAt)
		require.Nil(t, settled.WriteBack.CommittedAt)
	})
	require.NoError(t, err)
	require.Equal(t, 4, result.Applied)
	after := f.plan(t, plan)
	require.NotNil(t, after.WriteBack.CommittedAt)
	require.Equal(t, settled.WriteBack.SettledAt, after.WriteBack.SettledAt)
	require.Equal(t, settled.AppliedAt, after.AppliedAt)
}

func writeBackAfterSourceCommit(t *testing.T, f *followFixture, plan *merge.Plan, afterCommit func()) (*merge.ApplyResult, error) {
	t.Helper()
	ctx := context.Background()
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, event *event.CommandSucceededEvent) {
		if event.CommandName == "commitTransaction" {
			afterCommit()
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(ctx) }()
	return f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{
		Database: client.Database(f.source.Name()),
	})
}
