package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func reproImagesEnabledOn(t *testing.T, f *followFixture, collection string) bool {
	t.Helper()
	specs, err := f.source.ListCollectionSpecifications(context.Background(), bson.M{"name": collection})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	enabled, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
	return enabled
}

// An online import with --enable-source-images that fails at BeginImport
// (the name is held by another import in progress) has already altered the
// source's collections, and neither a result nor the error says so.
func TestOnlineImport_ImagesAreNotEnabledBeforeTheProjectIsReserved(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	// A concurrent import holds the name. GetProjectByName does not see it.
	_, err := f.services.Projects.BeginImport(ctx, f.project)
	require.NoError(t, err)

	result, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
		URI: f.sourceURI, Database: f.source.Name(), Project: f.project, EnableSourceImages: true,
	})
	require.Error(t, err)
	t.Logf("import result: %v", result)
	t.Logf("import error: %v", err)
	t.Logf("images enabled on source catalog after the failed import: %v", reproImagesEnabledOn(t, f, "catalog"))
	require.Contains(t, err.Error(), "failed to create project")
	require.NotContains(t, err.Error(), "image")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"),
		"a failed import left changeStreamPreAndPostImages enabled on the source and did not report it")
}

// source follow --enable-source-images alters the source before Follow
// checks its own preconditions.
func TestFollow_ImagesAreNotEnabledWhenFollowingCannotStart(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	err := f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, EnableSourceImages: true})
	require.Error(t, err)
	t.Logf("second follow error: %v", err)
	on := reproImagesEnabledOn(t, f, "catalog")
	t.Logf("images enabled on source catalog after the refused follow: %v", on)
	require.NoError(t, stop())
	require.NotContains(t, err.Error(), "image")
	require.False(t, on, "a refused follow left changeStreamPreAndPostImages enabled on the source and did not report it")
}
