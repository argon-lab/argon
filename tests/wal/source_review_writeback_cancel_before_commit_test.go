package wal_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// The caller's context ends after the transaction body has finished and
// before the commit is sent. The driver aborts the transaction without ever
// sending commitTransaction, so nothing can have been written.
func TestWriteBack_CancelBeforeCommitReturnsThePlanToPending(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)
	before := f.sourceState(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writes, commits, aborts atomic.Int32
	monitor := &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			switch e.CommandName {
			case "commitTransaction":
				commits.Add(1)
			case "abortTransaction":
				aborts.Add(1)
			}
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if e.CommandName == "update" || e.CommandName == "delete" || e.CommandName == "insert" {
				// The reply to the last write of the body has been read.
				if writes.Add(1) == int32(len(plan.Changes)) {
					cancel()
				}
			}
		},
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()

	_, err = f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{
		Database: client.Database(f.source.Name()),
	})
	t.Logf("ApplyToSource error: %v", err)
	t.Logf("write commands: %d, commitTransaction sent: %d, abortTransaction sent: %d", writes.Load(), commits.Load(), aborts.Load())
	require.Error(t, err)
	require.EqualValues(t, 4, writes.Load(), "the whole body ran")
	require.Zero(t, commits.Load(), "commitTransaction was never sent")
	require.EqualValues(t, 1, aborts.Load(), "the driver aborted the transaction")
	require.Equal(t, before, f.sourceState(t, nil), "nothing was written to the source")

	stuck := f.plan(t, plan)
	t.Logf("plan status right after the never-committed transaction: %s", stuck.Status)

	// Following catches up past the moment of the abort; a later source write
	// proves the stream is beyond it.
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err = f.source.Collection("catalog").UpdateOne(context.Background(), bson.M{"_id": "item-15"}, bson.M{"$inc": bson.M{"stock": 1}})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	after := f.plan(t, plan)
	t.Logf("plan status after following caught up: %s (write_back record present: %v)", after.Status, after.WriteBack != nil)

	_, retryErr := f.services.WriteBackMerge(context.Background(), plan.ID, "", f.sourceURI)
	t.Logf("retry: %v", retryErr)

	// Expected by the finding: a transaction that was never committed puts
	// the plan back to pending.
	require.Equal(t, merge.StatusPending, stuck.Status, "the transaction was aborted without a commit attempt; the plan should be pending again")
	require.Equal(t, merge.StatusPending, after.Status)
}
