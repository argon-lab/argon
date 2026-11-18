package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestWriteBack_BeforeImageReadFailureLeavesPlanPending(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	catalog := f.source.Collection("catalog")
	id := bson.M{"_id": "one"}
	_, err := catalog.InsertOne(ctx, bson.D{{Key: "_id", Value: "one"}, {Key: "stock", Value: int32(10)}, {Key: "title", Value: "One"}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	sandbox, sandboxDB := f.agentSandbox(t, "stock-edits")
	_, err = sandboxDB.Collection("catalog").UpdateOne(ctx, id, bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)

	// The source now differs only by BSON field order, which an inexact
	// comparison would miss. Fail only the metadata client's history read.
	reordered := bson.D{{Key: "_id", Value: "one"}, {Key: "title", Value: "One"}, {Key: "stock", Value: int32(10)}}
	_, err = catalog.ReplaceOne(ctx, id, reordered)
	require.NoError(t, err)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(metadataDeploymentURI))
	require.NoError(t, err)
	history, err := wal.NewService(client.Database(f.metadataName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, history.Close()) })
	require.NoError(t, client.Disconnect(ctx))
	writer := merge.NewService(f.services.Client.Database(f.metadataName), history, f.services.Branches, f.services.Materializer, f.services.Client)
	result, err := writer.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: f.source})
	require.ErrorContains(t, err, "could not read write-back before-image")
	require.ErrorIs(t, err, mongo.ErrClientDisconnected)
	require.Nil(t, result)
	stored := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, stored.Status)
	require.Nil(t, stored.WriteBack)
	var got bson.D
	require.NoError(t, catalog.FindOne(ctx, id).Decode(&got))
	require.Equal(t, reordered, got)

	// A healthy metadata connection retrieves the exact bytes and rejects
	// the stale plan because the source document's field order changed.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorAs(t, err, new(*merge.ConflictError))
}

func TestWriteBack_ReclaimedBeforeImageLeavesPlanPending(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	sandbox, sandboxDB := f.agentSandbox(t, "stock-edits")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)
	before := f.sourceState(t, nil)
	assertRefused := func(t *testing.T) {
		t.Helper()
		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
		require.ErrorContains(t, err, "exact write-back before-image for catalog/item-00")
		require.Nil(t, result)
		stored := f.plan(t, plan)
		require.Equal(t, merge.StatusPending, stored.Status)
		require.Nil(t, stored.WriteBack)
		require.Equal(t, before, f.sourceState(t, nil))
	}

	// GC first records its cutoff. A read during deletion might see only
	// some of the entries below it, so they cannot prove exact BSON order.
	require.NoError(t, f.services.WAL.RecordReclaimFloor(ctx, main.ID, "catalog", plan.TargetHead))
	history, err := f.services.WAL.GetDocumentHistory(main.ID, "catalog", "item-00", main.BaseLSN+1, plan.TargetHead)
	require.NoError(t, err)
	require.NotEmpty(t, history)
	t.Run("floor recorded before deletion", assertRefused)

	report, err := f.services.RunGC(ctx, main.ProjectID, 0, false)
	require.NoError(t, err)
	require.Positive(t, report.EntriesRemoved)
	history, err = f.services.WAL.GetDocumentHistory(main.ID, "catalog", "item-00", main.BaseLSN+1, plan.TargetHead)
	require.NoError(t, err)
	require.Empty(t, history)
	t.Run("history reclaimed", assertRefused)
}
