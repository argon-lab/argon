package wal_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Check the whole suffix, including duplicate rows that converge to the same
// materialized state. A nil post-image represents a deletion.
func checkFaultSourceRows(t *testing.T, f *followFixture, after int64, want map[string]bson.M) {
	t.Helper()
	rows := f.followedEntries(t, after)
	require.Len(t, rows, len(want))
	seen := make(map[string]bool)
	for _, row := range rows {
		post, ok := want[row.DocumentID]
		require.True(t, ok, "unexpected WAL key %s", row.DocumentID)
		require.False(t, seen[row.DocumentID], "duplicate WAL key %s", row.DocumentID)
		seen[row.DocumentID] = true
		require.Equal(t, "items", row.Collection)
		if post == nil {
			require.Equal(t, wal.OpDelete, row.Operation)
			require.Empty(t, row.PostImage)
			continue
		}
		require.Equal(t, wal.OpPut, row.Operation)
		var actual bson.M
		require.NoError(t, bson.Unmarshal(row.PostImage, &actual))
		require.Equal(t, post, actual)
	}
}

func faultSourceItem(key string, value int32) bson.M {
	return bson.M{"_id": key, "v": value}
}

func TestSourceFaultResumeTokenHistoryLost(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, uri string) {
		ctx := t.Context()
		f := newFollowFixture(t, uri)
		items := f.source.Collection("items")
		_, err := items.InsertMany(ctx, []any{faultSourceItem("a", 1), faultSourceItem("b", 1)})
		require.NoError(t, err)
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		_, err = items.UpdateByID(ctx, "a", bson.M{"$inc": bson.M{"v": 1}})
		require.NoError(t, err)
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		lastGood := f.status(t)
		checkpointState := f.mainState(t, lastGood.HeadLSN)

		_, err = items.UpdateByID(ctx, "a", bson.M{"$inc": bson.M{"v": 1}})
		require.NoError(t, err)
		_, err = items.DeleteOne(ctx, bson.M{"_id": "b"})
		require.NoError(t, err)
		_, err = items.InsertOne(ctx, faultSourceItem("c", 4))
		require.NoError(t, err)
		// Simulate the server rejecting the saved resume token the same way
		// as TestResync_AfterLostPosition. A new replica set keeps its oplog.
		var openedWithResume atomic.Bool
		err = f.followUntilItStops(t, walcli.FollowSourceOptions{
			Watch: func(_ context.Context, _ *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				openedWithResume.Store(opts.ResumeAfter != nil)
				return nil, mongo.CommandError{Code: 286, Name: "ChangeStreamHistoryLost"}
			},
		})
		require.ErrorContains(t, err, "ChangeStreamHistoryLost")
		require.True(t, openedWithResume.Load(), "fault must hit a resume attempt")
		gap := f.status(t)
		require.Equal(t, "degraded", gap.State)
		require.Equal(t, lastGood.HeadLSN, gap.HeadLSN)
		require.Equal(t, lastGood.ConsistentLSN, gap.ConsistentLSN)
		require.Equal(t, checkpointState, f.mainState(t, 0))
		checkFaultSourceRows(t, f, lastGood.HeadLSN, nil)

		resynced := f.resync(t, f.services, walcli.ResyncSourceOptions{})
		f.requireResynced(t, gap, resynced)
		checkFaultSourceRows(t, f, lastGood.HeadLSN, map[string]bson.M{
			"a": faultSourceItem("a", 3), "b": nil, "c": faultSourceItem("c", 4),
		})
		stop = f.follow(t, f.services, walcli.FollowSourceOptions{})
		_, err = items.UpdateByID(ctx, "c", bson.M{"$inc": bson.M{"v": 1}})
		require.NoError(t, err)
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		checkFaultSourceRows(t, f, resynced.ConsistentLSN, map[string]bson.M{"c": faultSourceItem("c", 5)})
		f.requireMainEqualsSource(t, 0, nil, "resumed after the marked gap")
	})
}

func TestSourceFaultWriteBackInterruptedAfterFirstWrite(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, uri string) {
		ctx := t.Context()
		f := newFollowFixture(t, uri)
		_, err := f.source.Collection("items").InsertMany(ctx, []any{faultSourceItem("a", 1), faultSourceItem("b", 2)})
		require.NoError(t, err)
		f.importOnline(t, walcli.OnlineImport{})
		base := f.main(t).HeadLSN
		before := f.sourceState(t, nil)
		info, sandboxDB := f.agentSandbox(t, "edited")
		items := sandboxDB.Collection("items")
		_, err = items.UpdateByID(ctx, "a", bson.M{"$inc": bson.M{"v": 4}})
		require.NoError(t, err)
		_, err = items.DeleteOne(ctx, bson.M{"_id": "b"})
		require.NoError(t, err)
		_, err = items.InsertOne(ctx, faultSourceItem("c", 6))
		require.NoError(t, err)
		plan := f.preview(t, info.BranchID)
		require.Len(t, plan.Changes, 3)
		approval := approvalFor(t, plan)

		writeCtx, interrupt := context.WithCancel(ctx)
		defer interrupt()
		var writes, commits atomic.Int32
		monitor := &event.CommandMonitor{
			Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
				switch e.CommandName {
				case "insert", "update", "delete":
					// The first write reached the server in the uncommitted
					// transaction. The remaining two changes have not run.
					if writes.Add(1) == 1 {
						interrupt()
					}
				case "commitTransaction":
					commits.Add(1)
				}
			},
		}
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetMonitor(monitor))
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
		_, err = f.services.Merge.ApplyToSource(writeCtx, plan.ID, "", merge.WriteBackTarget{
			Database: client.Database(f.source.Name()),
		}, approval)
		require.Error(t, err)
		require.EqualValues(t, 1, writes.Load())
		require.Zero(t, commits.Load())
		require.Equal(t, before, f.sourceState(t, nil))
		require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
		require.Equal(t, base, f.main(t).HeadLSN)

		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", uri, approval)
		require.NoError(t, err)
		require.Equal(t, 3, result.Applied)
		_, err = f.services.WriteBackMerge(ctx, plan.ID, "", uri, approval)
		require.ErrorContains(t, err, "not pending")
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		checkFaultSourceRows(t, f, base, map[string]bson.M{
			"a": faultSourceItem("a", 5), "b": nil, "c": faultSourceItem("c", 6),
		})
		f.requireMainEqualsSource(t, 0, nil, "retried source transaction")
		require.Equal(t, merge.StatusApplied, f.plan(t, plan).Status)
	})
}
