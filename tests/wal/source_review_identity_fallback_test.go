package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A replica-set name says almost nothing about which deployment answers:
// "rs0" is everywhere, and staging is often a restore of production. The
// check never accepts a name on its own.
func TestSource_IdentityNeverRestsOnTheSetNameAlone(t *testing.T) {
	recorded := primitive.NewObjectID().Hex()
	withID := &source.Binding{SetName: "rs0", ReplicaSetID: recorded, Hosts: []string{"a:27017"}, Display: "mongodb://a:27017/shop"}
	require.NoError(t, withID.Verify(source.Identity{SetName: "rs0", ReplicaSetID: recorded}))
	require.NoError(t, withID.Verify(source.Identity{SetName: "renamed", ReplicaSetID: recorded, Hosts: []string{"z:1"}}), "the ID is the identity")
	require.ErrorContains(t, withID.Verify(source.Identity{SetName: "rs0", ReplicaSetID: primitive.NewObjectID().Hex(), Hosts: []string{"a:27017"}}), "different deployment")
	// The binding recorded an ID and this connection cannot read one: the
	// same name and even the same hosts do not make up for it.
	err := withID.Verify(source.Identity{SetName: "rs0", Hosts: []string{"a:27017"}})
	require.ErrorContains(t, err, "replSetGetConfig")
	require.Equal(t, "replica-set ID", withID.Strength())

	// A binding made without the privilege to read the ID: the name and at
	// least one member host must match.
	byName := &source.Binding{SetName: "rs0", Hosts: []string{"prod-1:27017", "prod-2:27017"}, Display: "mongodb://prod-1:27017/shop"}
	require.NoError(t, byName.Verify(source.Identity{SetName: "rs0", Hosts: []string{"PROD-2:27017", "prod-3:27017"}}))
	require.ErrorContains(t, byName.Verify(source.Identity{SetName: "rs0", Hosts: []string{"staging-1:27017"}}), "none of the members")
	require.ErrorContains(t, byName.Verify(source.Identity{SetName: "rs0"}), "none of the members")
	require.ErrorContains(t, byName.Verify(source.Identity{SetName: "rs1", Hosts: []string{"prod-1:27017"}}), `replica set "rs1"`)
	require.ErrorContains(t, (&source.Binding{SetName: "rs0"}).Verify(source.Identity{SetName: "rs0", Hosts: []string{"x:1"}}), "none of the members", "a binding with neither ID nor hosts proves nothing")
	require.Equal(t, "replica-set name and member hosts", byName.Strength())
}

// End to end against two real deployments. The project is imported from one;
// the other holds a copy of the same data under the same database name, as a
// staging restore of production would. The stored binding is put into the
// state an import by a user without replSetGetConfig records, claiming the
// other deployment's set name. Write-back, follow and --enable-source-images
// pointed at the other deployment are all refused, and nothing changes there.
func TestSource_AnotherDeploymentWithTheSameSetNameIsRefused(t *testing.T) {
	deployments := sourceDeployments()
	if len(deployments) < 2 {
		t.Skip("set ARGON_TEST_SOURCE_MONGODB_URI to a second replica set")
	}
	ctx := context.Background()
	other := deployments["source on a separate deployment"]
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	otherClient, err := mongo.Connect(ctx, options.Client().ApplyURI(other))
	require.NoError(t, err)
	otherDB := otherClient.Database(f.source.Name())
	t.Cleanup(func() {
		_ = otherDB.Drop(context.Background())
		_ = otherClient.Disconnect(context.Background())
	})
	otherIdentity, err := source.Identify(ctx, otherClient)
	require.NoError(t, err)
	cursor, err := f.source.Collection("catalog").Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []interface{}
	for cursor.Next(ctx) {
		docs = append(docs, bson.Raw(append([]byte(nil), cursor.Current...)))
	}
	require.NoError(t, cursor.Err())
	_, err = otherDB.Collection("catalog").InsertMany(ctx, docs)
	require.NoError(t, err)
	untouched := func(label string) {
		t.Helper()
		n, err := otherDB.Collection("catalog").CountDocuments(ctx, bson.M{"$or": bson.A{bson.M{"_id": "item-new"}, bson.M{"note": "sold out"}}})
		require.NoError(t, err)
		require.Zero(t, n, "%s: the plan reached the other deployment", label)
		n, err = otherDB.Collection("catalog").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		require.EqualValues(t, len(docs), n, label)
		specs, err := otherDB.ListCollectionSpecifications(ctx, bson.M{"name": "catalog"})
		require.NoError(t, err)
		enabled, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
		require.False(t, enabled, "%s: the other deployment's collection was altered", label)
		require.Equal(t, merge.StatusPending, f.plan(t, plan).Status, label)
	}

	bindings := f.services.Client.Database(f.metadataName).Collection("wal_sources")
	main := f.main(t)
	// The name-only binding, claiming the other deployment's set name. Its
	// recorded hosts are still the real source's.
	_, err = bindings.UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{
		"$set": bson.M{"set_name": otherIdentity.SetName}, "$unset": bson.M{"replica_set_id": ""}})
	require.NoError(t, err)

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", other)
	require.ErrorContains(t, err, "none of the members")
	untouched("write-back")
	err = f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: other, EnableSourceImages: true})
	require.ErrorContains(t, err, "none of the members")
	untouched("follow with --enable-source-images")
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: other})
	require.ErrorContains(t, err, "none of the members")
	untouched("resync")

	// Back on the real source the name-only binding is accepted on name plus
	// hosts, and takes the replica-set ID now that it can be read.
	_, err = bindings.UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{"$set": bson.M{"set_name": "rs0"}})
	require.NoError(t, err)
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "replica-set name and member hosts", status.Identity)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	status, err = f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "replica-set ID", status.Identity)
	untouched0, err := otherDB.Collection("catalog").CountDocuments(ctx, bson.M{"_id": "item-new"})
	require.NoError(t, err)
	require.Zero(t, untouched0)
}
