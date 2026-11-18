package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// When a source with a weak binding replaces every member, a privileged
// connection cannot prove that it reached the same source. Detaching and
// importing into a new project records the ID with that connection.
func TestSource_ReimportAfterAllWeaklyBoundHostsChange(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	identity, err := source.Identify(ctx, f.sourceClient)
	require.NoError(t, err)
	require.NotEmpty(t, identity.ReplicaSetID, "this connection may run replSetGetConfig")

	// The state an import by a user without replSetGetConfig records, for a
	// source whose members have since all been replaced: same set name, no
	// ID, and recorded hosts that no longer exist.
	bindings := f.services.Client.Database(f.metadataName).Collection("wal_sources")
	main := f.main(t)
	_, err = bindings.UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{
		"$set":   bson.M{"hosts": bson.A{"old-node-1:27017", "old-node-2:27017"}},
		"$unset": bson.M{"replica_set_id": ""}})
	require.NoError(t, err)

	checkRefusal := func(err error) {
		t.Helper()
		require.ErrorContains(t, err, "none of the members")
		require.ErrorContains(t, err, "argon source detach -p <project> --yes")
		require.ErrorContains(t, err, "import online into a new project with a user allowed to run replSetGetConfig")
	}
	stored, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	checkRefusal(stored.Verify(identity))
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	checkRefusal(err)
	checkRefusal(f.followUntilItStops(t, walcli.FollowSourceOptions{}))
	unchanged, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Equal(t, stored, unchanged)

	require.NoError(t, f.services.DetachSource(ctx, f.project))
	detached, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	require.Nil(t, detached)
	f.project += "-fresh"
	f.importOnline(t, walcli.OnlineImport{})
	fresh, err := f.services.Sources.Get(ctx, f.main(t).ID)
	require.NoError(t, err)
	require.Equal(t, identity.ReplicaSetID, fresh.ReplicaSetID)
	status := f.status(t)
	require.Equal(t, "replica-set ID", status.Identity)
	require.Empty(t, status.WriteBackUnavailableReason)
}
