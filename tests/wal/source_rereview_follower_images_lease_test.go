package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFollow_LeaseRefusalLeavesSourceAndBindingUnchanged(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})

	other := f.restart(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A connection with more privileges could update an older binding that
	// was imported without the privilege to read the set's ID.
	bindings := f.services.Client.Database(f.metadataName).Collection("wal_sources")
	_, err := bindings.UpdateOne(ctx, bson.M{"_id": f.main(t).ID}, bson.M{"$unset": bson.M{"replica_set_id": ""}})
	require.NoError(t, err)
	require.Equal(t, "replica-set name and member hosts", f.status(t).Identity)

	err = other.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, EnableSourceImages: true})
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.ErrorContains(t, err, "source follow is already running")
	require.False(t, reproImagesEnabledOn(t, f, "catalog"))
	require.Equal(t, "replica-set name and member hosts", f.status(t).Identity)
	require.NoError(t, stop())

	stop = f.follow(t, other, walcli.FollowSourceOptions{})
	require.Equal(t, "replica-set ID", f.status(t).Identity)
	require.NoError(t, stop())
}
