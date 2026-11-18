package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
)

func TestFollow_LeaseRefusalLeavesSourceAndBindingUnchanged(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})

	other := f.restart(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	binding, err := f.services.Sources.Get(ctx, f.main(t).ID)
	require.NoError(t, err)
	require.NotEmpty(t, binding.ReplicaSetID)

	err = other.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, EnableSourceImages: true})
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.ErrorContains(t, err, "source follow is already running")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	after, err := f.services.Sources.Get(ctx, f.main(t).ID)
	require.NoError(t, err)
	require.Equal(t, binding, after)
	require.NoError(t, stop())

	stop = f.follow(t, other, walcli.FollowSourceOptions{})
	require.Equal(t, "replica-set ID", f.status(t).Identity)
	require.NoError(t, stop())
}
