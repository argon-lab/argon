package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestOnlineImport_DryRunAndPreviewSkipViews(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))

	dry := f.importOnline(t, walcli.OnlineImport{DryRun: true, EnableSourceImages: true})
	require.True(t, dry.Online)
	require.Equal(t, []string{"catalog"}, dry.Collections)
	require.Empty(t, dry.ProjectID)
	require.Empty(t, dry.BranchID)
	require.Empty(t, dry.ImagesEnabled)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	projects, err := f.services.Projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)

	for _, online := range []bool{false, true} {
		data, err := walcli.ImportPreview(ctx, f.sourceURI, f.source.Name(), online)
		require.NoError(t, err)
		preview := data.(*importer.ImportPreview)
		require.Len(t, preview.Collections, 1)
		require.Equal(t, "catalog", preview.Collections[0].Name)
		require.Equal(t, dry.ImportedDocs, preview.TotalDocuments)
	}
	real := f.importOnline(t, walcli.OnlineImport{})
	require.Equal(t, dry.Collections, real.Collections)
	require.Equal(t, dry.ImportedDocs, real.ImportedDocs)
}

func TestOnlineImport_DryRunAndPreviewIncludeDottedCollections(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	for _, name := range []string{"config.flags", "admin.users", "local.cache"} {
		_, err := f.source.Collection(name).InsertOne(ctx, bson.M{"_id": 1, "v": 1})
		require.NoError(t, err)
	}

	dry := f.importOnline(t, walcli.OnlineImport{DryRun: true})
	data, err := walcli.ImportPreview(ctx, f.sourceURI, f.source.Name(), true)
	require.NoError(t, err)
	preview := data.(*importer.ImportPreview)
	previewNames := make([]string, 0, len(preview.Collections))
	for _, collection := range preview.Collections {
		previewNames = append(previewNames, collection.Name)
	}
	real := f.importOnline(t, walcli.OnlineImport{})
	require.ElementsMatch(t, []string{"admin.users", "catalog", "config.flags", "local.cache"}, real.Collections)
	require.ElementsMatch(t, real.Collections, previewNames)
	require.ElementsMatch(t, real.Collections, dry.Collections)
	require.Equal(t, real.ImportedDocs, dry.ImportedDocs)
	require.Equal(t, real.ImportedDocs, preview.TotalDocuments)
}

func TestOnlineImport_DryRunRefusesOwnedDatabase(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	for _, database := range []string{f.metadataName, "argon_br_example"} {
		opts := walcli.OnlineImport{URI: metadataDeploymentURI, Database: database, Project: f.project}
		_, importErr := f.services.ImportDatabaseOnline(ctx, opts)
		require.ErrorContains(t, importErr, "cannot be followed as a source")
		opts.DryRun = true
		_, dryErr := f.services.ImportDatabaseOnline(ctx, opts)
		require.EqualError(t, dryErr, importErr.Error())
	}
}
