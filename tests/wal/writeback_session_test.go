package wal_test

import (
	"context"
	"sync"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestWriteBack_ReusedSessionAttribution(t *testing.T) {
	ctx := t.Context()
	f, branch := planFixture(t)
	insertPlan := func(id string) *merge.Plan {
		t.Helper()
		p, err := f.services.Merge.PreparePlan(ctx, branch)
		require.NoError(t, err)
		doc, err := bson.Marshal(bson.D{{Key: "_id", Value: id}})
		require.NoError(t, err)
		p.Changes = []merge.Change{{Collection: "catalog", DocumentID: id, DocumentRaw: doc}}
		require.NoError(t, f.services.Merge.SavePlan(ctx, p))
		return f.plan(t, p)
	}
	one, two := insertPlan("parcel-a"), insertPlan("parcel-b")
	meta, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI))
	require.NoError(t, err)
	closeMeta := sync.OnceFunc(func() { require.NoError(t, meta.Disconnect(context.Background())) })
	t.Cleanup(closeMeta)
	writer := merge.NewService(meta.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, meta)
	var loseMetadata bool
	var numbers []int64
	client := electionClient(t, f.sourceURI, &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.CommandName == "commitTransaction" {
				numbers = append(numbers, e.Command.Lookup("txnNumber").Int64())
			}
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if e.CommandName == "commitTransaction" && loseMetadata {
				closeMeta()
			}
		},
	})
	db := client.Database(f.source.Name())
	ordinary := func(id string) {
		t.Helper()
		err := client.UseSession(ctx, func(sc mongo.SessionContext) error {
			_, err := sc.WithTransaction(sc, func(tx mongo.SessionContext) (any, error) {
				return db.Collection("catalog").InsertOne(tx, bson.M{"_id": id})
			})
			return err
		})
		require.NoError(t, err)
	}
	target := merge.WriteBackTarget{Database: db}
	ordinary("before")
	r, err := writer.ApplyToSource(ctx, one.ID, "", target, approvalFor(t, one))
	require.NoError(t, err)
	require.Equal(t, 1, r.Applied)
	first := f.plan(t, one).WriteBack
	ordinary("between")
	// The second transaction commits, then the writer loses metadata access.
	// Following must settle that claim and attribute the neighboring transactions.
	loseMetadata = true
	r, err = writer.ApplyToSource(ctx, two.ID, "", target, approvalFor(t, two))
	require.ErrorContains(t, err, "could not be marked applied")
	require.Equal(t, 1, r.Applied)
	claimed := f.plan(t, two)
	require.Equal(t, merge.StatusWritingBack, claimed.Status)
	second := claimed.WriteBack
	ordinary("after")
	require.Len(t, numbers, 5)
	require.Equal(t, first.Session, second.Session)
	require.Equal(t, &numbers[0], first.AfterTxn)
	require.Equal(t, &numbers[1], first.CommitTxn)
	require.Equal(t, &numbers[2], second.AfterTxn)
	require.Nil(t, second.CommitTxn)
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	entries := f.followedEntries(t, one.TargetHead)
	require.Len(t, entries, 5)
	for i, id := range []string{"before", "parcel-a", "between", "parcel-b", "after"} {
		require.Equal(t, id, entries[i].DocumentID)
	}
	require.NotContains(t, entries[0].TxnID, "merge:")
	require.Equal(t, "merge:"+one.ID.Hex(), entries[1].TxnID)
	require.NotContains(t, entries[2].TxnID, "merge:")
	require.Equal(t, "merge:"+two.ID.Hex(), entries[3].TxnID)
	require.NotContains(t, entries[4].TxnID, "merge:")
	for _, p := range []*merge.Plan{one, two} {
		done := f.plan(t, p)
		require.Equal(t, merge.StatusApplied, done.Status)
		require.NotNil(t, done.WriteBack.SettledAt)
	}
	require.Nil(t, f.plan(t, two).WriteBack.CommittedAt)

	// A session-only claim remains the fallback before the next bound.
	records := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	_, err = records.UpdateByID(ctx, one.ID, bson.M{"$unset": bson.M{"write_back.after_txn": "", "write_back.commit_txn": ""}})
	require.NoError(t, err)
	for index, want := range map[int]*merge.Plan{1: one, 3: two} {
		matched, err := f.services.Merge.LookupWriteBack(ctx, one.TargetBranchID, first.Session, numbers[index])
		require.NoError(t, err)
		require.NotNil(t, matched)
		require.Equal(t, want.ID, matched.ID)
	}
}
