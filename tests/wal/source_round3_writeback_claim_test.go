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

func TestWriteBack_ClaimTransitionsUseMajority(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	sandbox, sandboxDB := f.agentSandbox(t, "stock-edits")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)

	var lock sync.Mutex
	var concerns []bson.RawValue
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI+"/?w=1").SetMonitor(&event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.DatabaseName != f.metadataName || e.CommandName != "update" || e.Command.Lookup("update").StringValue() != "wal_merge_plans" {
				return
			}
			lock.Lock()
			defer lock.Unlock()
			concern := e.Command.Lookup("writeConcern", "w")
			concerns = append(concerns, bson.RawValue{Type: concern.Type, Value: append([]byte(nil), concern.Value...)})
		},
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(ctx)) })
	writer := merge.NewService(client.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, client)
	target := merge.WriteBackTarget{Database: f.source}

	result, err := writer.ApplyToSource(ctx, plan.ID, "", target)
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	require.Equal(t, merge.StatusApplied, f.plan(t, plan).Status)

	// Main still has the preview's before-image. A fresh plan claims the
	// change, then returns to pending when the comparison disagrees.
	stale := f.preview(t, sandbox.BranchID)
	_, err = writer.ApplyToSource(ctx, stale.ID, "", target)
	require.ErrorAs(t, err, new(*merge.ConflictError))
	require.Equal(t, merge.StatusPending, f.plan(t, stale).Status)
	lock.Lock()
	defer lock.Unlock()
	require.Len(t, concerns, 4, "claim and applied mark, then claim and return to pending")
	for _, concern := range concerns {
		w, ok := concern.StringValueOK()
		require.True(t, ok, "expected string write concern, got %v", concern)
		require.Equal(t, "majority", w)
	}
}

func TestWriteBack_ClaimWithoutAcknowledgmentLeavesSourceUntouched(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	sandbox, sandboxDB := f.agentSandbox(t, "stock-edits")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	before := f.sourceState(t, nil)

	dialer := &lostAckDialer{command: "update"}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI).SetDialer(dialer))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(ctx)) })
	writer := merge.NewService(client.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, client)
	result, err := writer.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: f.source})
	require.ErrorContains(t, err, "could not claim plan")
	require.ErrorContains(t, err, "waiting for replication")
	require.Nil(t, result)
	require.EqualValues(t, 1, dialer.acknowledgments.Load())
	require.Equal(t, before, f.sourceState(t, nil))
	require.Equal(t, merge.StatusWritingBack, f.plan(t, plan).Status, "the metadata server applied the claim locally")
}
