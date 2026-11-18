package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestOnlineImport_ResyncAndFollowExplicitNullID(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	catalog := f.source.Collection("catalog")
	initial := bson.M{"_id": nil, "stock": int32(9)}
	_, err := catalog.InsertOne(ctx, initial)
	require.NoError(t, err)

	project := f.project + "-null"
	_, err = f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
		URI: f.sourceURI, Database: f.source.Name(), Project: project,
	})
	require.NoError(t, err)
	imported, err := f.services.SourceStatus(ctx, project)
	require.NoError(t, err)
	importedProject, err := f.services.Projects.GetProjectByName(project)
	require.NoError(t, err)
	importedBranch, err := f.services.Branches.GetBranch(importedProject.ID, "main")
	require.NoError(t, err)
	key := wal.DocumentIDString(nil)
	doc, err := f.services.Materializer.MaterializeDocumentAtLSN(importedBranch, "catalog", key, imported.ConsistentLSN)
	require.NoError(t, err)
	require.Equal(t, initial, doc)

	result := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
	require.EqualValues(t, 1, result.Puts)
	require.Zero(t, result.Deletes, "pruning must find the null ID on the source")
	require.Equal(t, initial, f.mainState(t, result.ConsistentLSN)["catalog"][key])

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	filter := bson.M{"_id": nil}
	remove := func() {
		t.Helper()
		deleted, err := catalog.DeleteOne(ctx, filter)
		require.NoError(t, err)
		require.EqualValues(t, 1, deleted.DeletedCount)
		require.NotContains(t, f.mainState(t, f.waitCaughtUp(t).ConsistentLSN)["catalog"], key)
	}
	remove()

	_, err = catalog.InsertOne(ctx, initial)
	require.NoError(t, err)
	require.Equal(t, initial, f.mainState(t, f.waitCaughtUp(t).ConsistentLSN)["catalog"][key])

	updated, err := catalog.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"stock": int32(10)}})
	require.NoError(t, err)
	require.EqualValues(t, 1, updated.ModifiedCount)
	require.Equal(t, bson.M{"_id": nil, "stock": int32(10)}, f.mainState(t, f.waitCaughtUp(t).ConsistentLSN)["catalog"][key])
	remove()
	require.NoError(t, stop())
}
