package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// checkoutAs250 follows the checkout write path used before branches could
// follow a source. It replaces the ingest position before publishing checkout.
func (f *followFixture) checkoutAs250(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	branch := f.main(t)
	require.False(t, branch.IsLive())
	client := f.services.Client
	meta := client.Database(f.metadataName)
	dbName := wal.PhysicalDBName(branch.ID)
	physical := client.Database(dbName)

	state := f.mainState(t, branch.HeadLSN)
	require.NoError(t, physical.Drop(ctx))
	_, err := meta.Collection("wal_ingest_state").DeleteOne(ctx, bson.M{"_id": branch.ID})
	require.NoError(t, err)
	for collection, docs := range state {
		require.NoError(t, physical.RunCommand(ctx, bson.D{{Key: "create", Value: collection},
			{Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}}}).Err())
		batch := make([]interface{}, 0, len(docs))
		for _, doc := range docs {
			batch = append(batch, doc)
		}
		if len(batch) > 0 {
			_, err := physical.Collection(collection).InsertMany(ctx, batch)
			require.NoError(t, err)
		}
	}
	var reply bson.Raw
	require.NoError(t, physical.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&reply))
	seconds, ordinal, ok := reply.Lookup("operationTime").TimestampOK()
	require.True(t, ok)
	_, err = meta.Collection("wal_ingest_state").UpdateOne(ctx, bson.M{"_id": branch.ID},
		bson.M{"$set": bson.M{"start_at": primitive.Timestamp{T: seconds, I: ordinal + 1}}}, options.Update().SetUpsert(true))
	require.NoError(t, err)
	var version interface{} = branch.MutationVersion
	if branch.MutationVersion == 0 {
		version = bson.M{"$in": bson.A{nil, int64(0)}}
	}
	res, err := meta.Collection("wal_branches").UpdateOne(ctx,
		bson.M{"_id": branch.ID, "head_lsn": branch.HeadLSN, "is_deleted": false, "mutation_version": version, "state": bson.M{"$ne": "live"}},
		bson.M{"$set": bson.M{"physical_db": dbName, "state": "live", "checked_out_lsn": branch.HeadLSN}, "$inc": bson.M{"mutation_version": 1}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.MatchedCount, "2.5.0's CompareAndSetCheckout matched the followed main")
	return dbName
}

func TestSource_LegacyCheckoutRequiresReleaseAndResync(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	forEachSourceDeployment(t, testLegacyCheckoutRequiresReleaseAndResync)
}

func testLegacyCheckoutRequiresReleaseAndResync(t *testing.T, sourceURI string) {
	ctx := context.Background()
	f := newFollowFixture(t, sourceURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})

	sandboxInfo, sandboxDB := f.agentSandbox(t, "agent-run")
	_, err := sandboxDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"note": "checked plan"}})
	require.NoError(t, err)
	plan := f.preview(t, sandboxInfo.BranchID)
	require.Len(t, plan.Changes, 1)
	before := f.sourceState(t, nil)

	followCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.services.FollowSource(followCtx, f.project, walcli.FollowSourceOptions{URI: sourceURI, Ready: ready})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("following ended before the stream opened: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("following never opened the stream")
	}

	dbName := f.checkoutAs250(t)
	branch := f.main(t)
	require.True(t, branch.IsLive())
	require.Equal(t, dbName, branch.PhysicalDB)
	require.Equal(t, f.source.Name(), branch.Follows)
	other := f.restart(t)
	status, err := other.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.State)
	assert.Contains(t, status.Error, "argon release")

	var followErr error
	deadline := time.Now().Add(30 * time.Second)
wait:
	for time.Now().Before(deadline) {
		f.tick(t)
		select {
		case followErr = <-done:
			break wait
		case <-time.After(250 * time.Millisecond):
		}
	}
	var gap *ingest.CaptureError
	assert.ErrorAs(t, followErr, &gap, "following must stop as degraded after checkout")
	assert.ErrorContains(t, followErr, "argon release")

	result, err := other.WriteBackMerge(ctx, plan.ID, "", sourceURI, approvalFor(t, plan))
	assert.Nil(t, result)
	assert.ErrorContains(t, err, "degraded")
	assert.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
	result, err = other.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: f.source}, approvalFor(t, plan))
	require.ErrorContains(t, err, "degraded")
	require.Nil(t, result)
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
	assert.Equal(t, before, f.sourceState(t, nil))
	_, err = other.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: sourceURI, Force: true})
	assert.ErrorContains(t, err, "argon release")

	require.NoError(t, other.Checkout.Release(ctx, branch.ID))
	branch = f.main(t)
	require.False(t, branch.IsLive())
	require.Empty(t, branch.PhysicalDB)
	require.Equal(t, f.source.Name(), branch.Follows)
	names, err := f.services.Client.Database(dbName).ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.Empty(t, names)
	require.Equal(t, before, f.sourceState(t, nil))

	status, err = other.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", status.State)
	require.Contains(t, status.Error, "saved source position")
	_, err = other.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: sourceURI})
	require.NoError(t, err)
	f.requireMainEqualsSource(t, 0, nil, "after release and resync")
	stop := f.follow(t, other, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
}

func TestSource_MissingResumeTokenRequiresResync(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	sandbox, database := f.agentSandbox(t, "agent-run")
	_, err := database.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"note": "checked plan"}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	_, err = state.UpdateOne(ctx, bson.M{"_id": f.main(t).ID}, bson.M{"$unset": bson.M{"resume_token": ""}})
	require.NoError(t, err)

	status := f.status(t)
	assert.Equal(t, "degraded", status.State)
	assert.Contains(t, status.Error, "saved source position")
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	assert.Nil(t, result)
	assert.ErrorContains(t, err, "degraded")
	assert.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.NoError(t, err)
	f.requireMainEqualsSource(t, 0, nil, "after resync")
}

func TestResync_RebuildsMissingIngestState(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)
	require.NoError(t, f.services.Ingest.ClearResumeState(ctx, branch.ID))
	require.Equal(t, "degraded", f.status(t).State)

	_, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI,
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 13, Name: "Unauthorized"}
		},
	})
	require.ErrorContains(t, err, "Unauthorized")
	require.ErrorIs(t, f.ingestState().FindOne(ctx, bson.M{"_id": branch.ID}).Err(), mongo.ErrNoDocuments)

	f.resync(t, f.services, walcli.ResyncSourceOptions{})
	require.Equal(t, "stopped", f.status(t).State)
	f.requireMainEqualsSource(t, 0, nil, "after resync")
}
