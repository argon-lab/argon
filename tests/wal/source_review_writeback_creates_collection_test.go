package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// (b) A sandbox creates a brand-new collection; the plan is written back.
func TestWriteBack_DoesNotCreateACollection(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})

	info, sandboxDB := f.agentSandbox(t, "agent-run")
	_, err := sandboxDB.Collection("brand_new").InsertOne(ctx, bson.M{"_id": "n1", "v": 1})
	require.NoError(t, err)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 1)
	require.Equal(t, "brand_new", plan.Changes[0].Collection)

	before, err := f.source.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.NotContains(t, before, "brand_new")

	result, werr := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	after, err := f.source.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	t.Logf("write-back result=%+v err=%v; source collections before=%v after=%v", result, werr, before, after)
	require.NotContains(t, after, "brand_new", "write-back must not create a collection in the source")
}

// (a) Production drops a collection; the follower is degraded; a sandbox
// forked from the stale main inserts into it; write-back re-creates it.
func TestWriteBack_RefusedWhileFollowingIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("orders").InsertOne(ctx, bson.M{"_id": "o1", "total": 10})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	require.NoError(t, f.source.Collection("orders").Drop(ctx))
	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
	require.Error(t, followErr)
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", status.State)

	info, sandboxDB := f.agentSandbox(t, "agent-run")
	n, err := sandboxDB.Collection("orders").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	t.Logf("sandbox forked from degraded main still holds %d orders document(s)", n)
	_, err = sandboxDB.Collection("orders").InsertOne(ctx, bson.M{"_id": "o2", "total": 20})
	require.NoError(t, err)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 1)

	before, err := f.source.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.NotContains(t, before, "orders")

	result, werr := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	after, err := f.source.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	count, _ := f.source.Collection("orders").CountDocuments(ctx, bson.D{})
	t.Logf("follower=%s (%s); write-back result=%+v err=%v; source collections before=%v after=%v; orders docs in source=%d",
		status.State, status.Error, result, werr, before, after, count)
	require.NotContains(t, after, "orders", "write-back must not re-create a collection production dropped")
}
