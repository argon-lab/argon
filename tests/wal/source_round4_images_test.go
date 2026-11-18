package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
)

const sourceImagesOptInMessage = "--enable-source-images is experimental"

func TestOnlineImport_SourceImagesRequireOptIn(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	before := f.sourceState(t, nil)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	// Invalid input fails before EnableImages even if the general opt-in
	// check is bypassed, so only the import's own check can give this error.
	_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{EnableSourceImages: true})
	require.ErrorContains(t, err, sourceImagesOptInMessage)
	t.Setenv("MONGODB_URI", "invalid")
	_, err = walcli.DryRunOnlineImport(ctx, walcli.OnlineImport{EnableSourceImages: true})
	require.ErrorContains(t, err, sourceImagesOptInMessage)
	_, err = importer.DryRunOnline(ctx, importer.ImportOptions{Online: true, EnableSourceImages: true}, nil)
	require.ErrorContains(t, err, sourceImagesOptInMessage)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	result := f.importOnline(t, walcli.OnlineImport{EnableSourceImages: true})
	require.Equal(t, []string{"catalog"}, result.ImagesEnabled)
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
	require.Equal(t, before, f.sourceState(t, nil))
}

func TestFollow_SourceImagesRequireOptIn(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	before := f.sourceState(t, nil)

	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.NoError(t, stop())
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	// A missing project makes the option check observable independently of
	// the guard at collMod and of the command's generic check.
	err := f.services.FollowSource(ctx, f.project+"-missing", walcli.FollowSourceOptions{EnableSourceImages: true})
	require.ErrorContains(t, err, sourceImagesOptInMessage)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	stop = f.follow(t, f.services, walcli.FollowSourceOptions{EnableSourceImages: true})
	require.NoError(t, stop())
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
	require.Equal(t, before, f.sourceState(t, nil))
}

func TestSource_EnableImagesRequiresOptIn(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	changed, err := source.EnableImages(ctx, f.source, []string{"catalog"}, nil)
	require.ErrorContains(t, err, sourceImagesOptInMessage)
	require.Empty(t, changed)
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))

	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	changed, err = source.EnableImages(ctx, f.source, []string{"catalog"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"catalog"}, changed)
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
}
