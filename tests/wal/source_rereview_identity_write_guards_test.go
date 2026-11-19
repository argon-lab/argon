package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Two deployments that report the same set name and member host strings
// (Helm/compose defaults). The project was imported from one of them by a
// user who could not read the replica-set ID. Two real deployments in one
// network namespace cannot report identical strings, so the stored binding is
// given the other deployment's strings: from the binding's point of view that
// is exactly the state "my source and that deployment report the same name
// and hosts", and it holds no ID.
func TestWriteBack_WeakBindingRefusesSourceWrites(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	deployments := sourceDeployments()
	if len(deployments) < 2 {
		t.Skip("set ARGON_TEST_SOURCE_MONGODB_URI to a second replica set")
	}
	ctx := context.Background()
	staging := deployments["source on a separate deployment"]
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	prodIdentity, err := source.Identify(ctx, f.sourceClient)
	require.NoError(t, err)

	stagingClient, err := mongo.Connect(ctx, options.Client().ApplyURI(staging))
	require.NoError(t, err)
	stagingDB := stagingClient.Database(f.source.Name())
	t.Cleanup(func() {
		_ = stagingDB.Drop(context.Background())
		_ = stagingClient.Disconnect(context.Background())
	})
	stagingIdentity, err := source.Identify(ctx, stagingClient)
	require.NoError(t, err)
	require.NotEmpty(t, stagingIdentity.ReplicaSetID)
	require.NotEqual(t, prodIdentity.ReplicaSetID, stagingIdentity.ReplicaSetID)

	// Staging is a restore of production.
	cursor, err := f.source.Collection("catalog").Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.Raw
	require.NoError(t, cursor.All(ctx, &docs))
	for _, doc := range docs {
		_, err := stagingDB.Collection("catalog").InsertOne(ctx, doc)
		require.NoError(t, err)
	}

	bindings := f.services.Client.Database(f.metadataName).Collection("wal_sources")
	main := f.main(t)
	_, err = bindings.UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{
		"$set":   bson.M{"set_name": stagingIdentity.SetName, "hosts": stagingIdentity.Hosts},
		"$unset": bson.M{"replica_set_id": ""}})
	require.NoError(t, err)
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "replica-set name and member hosts", status.Identity)

	require.Contains(t, status.WriteBackUnavailableReason, "requires a replica-set ID recorded at import")

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", staging, approvalFor(t, plan))
	require.ErrorContains(t, err, "write-back refused")
	requireWeakBindingRemedy(t, err)
	storedPlan := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, storedPlan.Status)
	require.Nil(t, storedPlan.WriteBack)
	var after []bson.Raw
	cursor, err = stagingDB.Collection("catalog").Find(ctx, bson.D{})
	require.NoError(t, err)
	require.NoError(t, cursor.All(ctx, &after))
	require.ElementsMatch(t, docs, after)

	followCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = f.services.FollowSource(followCtx, f.project, walcli.FollowSourceOptions{URI: staging, EnableSourceImages: true})
	require.ErrorContains(t, err, "--enable-source-images refused")
	requireWeakBindingRemedy(t, err)
	specs, err := stagingDB.ListCollectionSpecifications(ctx, bson.M{"name": "catalog"})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	enabled, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
	require.False(t, enabled)

	stored, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Empty(t, stored.ReplicaSetID)
	require.Equal(t, stagingIdentity.SetName, stored.SetName)
	require.Equal(t, stagingIdentity.Hosts, stored.Hosts)
}

func requireWeakBindingRemedy(t *testing.T, err error) {
	t.Helper()
	require.ErrorContains(t, err, "a tunnel or another deployment can present the same replica-set name and member host strings")
	require.ErrorContains(t, err, "argon source detach -p <project> --yes")
	require.ErrorContains(t, err, "replSetGetConfig")
	require.ErrorContains(t, err, "clusterMonitor")
	require.ErrorContains(t, err, "into a new project")
}

func TestOnlineImport_ImagesRequireReplicaSetID(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	addr, blocked := startUnprivilegedProxy(t, "localhost:27017")
	uri := fmt.Sprintf("mongodb://%s/?directConnection=true", addr)
	_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
		URI: uri, Database: f.source.Name(), Project: f.project, EnableSourceImages: true,
	})
	require.Positive(t, blocked.Load())
	require.ErrorContains(t, err, "--enable-source-images refused")
	require.ErrorContains(t, err, "a tunnel or another deployment can present the same replica-set name and member host strings")
	require.ErrorContains(t, err, "replSetGetConfig")
	require.ErrorContains(t, err, "clusterMonitor")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	// The import refused before it reserved the project or changed the source.
	f.sourceURI = uri
	f.importOnline(t, walcli.OnlineImport{})
	require.Equal(t, "replica-set name and member hosts", f.status(t).Identity)
}
