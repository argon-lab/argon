//go:build unix

package wal_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const preflightExpiryDB = "ARGON_PREFLIGHT_EXPIRY_TEST_DB"
const preflightExpiryProject = "ARGON_PREFLIGHT_EXPIRY_PROJECT"
const preflightExit = 23

// The child exits as soon as resync opens the preflight stream. Acquisition
// has stored its lease, and the capture state still belongs to the clean stop.
func TestSource_ResyncOwnerExpiresDuringPreflight(t *testing.T) {
	useShortSourceLease(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if database := os.Getenv(preflightExpiryDB); database != "" {
		svc, err := walcli.NewServicesAt(metadataDeploymentURI, database)
		require.NoError(t, err)
		_, err = svc.ResyncSource(ctx, os.Getenv(preflightExpiryProject), walcli.ResyncSourceOptions{
			URI: reproSourceURI(t), Force: true,
			Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				os.Exit(preflightExit)
				return nil, nil
			},
		})
		t.Fatalf("resync returned before entering preflight: %v", err)
	}

	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	first := f.status(t)
	require.NoError(t, first.HealthError())
	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	savedOwner := f.expirePreflightOwner(t, ctx)
	expired := f.status(t)
	require.NotNil(t, expired.Owner)
	require.Equal(t, savedOwner.HeartbeatAt, expired.Owner.HeartbeatAt)
	require.Equal(t, first.Position, expired.Position)
	require.Equal(t, int32(10), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	require.Equal(t, int32(99), f.sourceState(t, nil)["catalog"]["item-00"]["stock"])
	require.True(t, expired.LeaseExpired)
	require.Equal(t, ingest.ReasonOwnerExpired, expired.Health.Reason)
	require.Error(t, expired.HealthError())
	require.False(t, f.deploymentCaptureHealthy(t, ctx))
}

func (f *followFixture) expirePreflightOwner(t *testing.T, ctx context.Context) *walcli.SourceOwner {
	t.Helper()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run", "^TestSource_ResyncOwnerExpiresDuringPreflight$")
	child.Env = append(os.Environ(), preflightExpiryDB+"="+f.metadataName, preflightExpiryProject+"="+f.project)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "%s", output)
	require.Equal(t, preflightExit, exit.ExitCode(), "%s", output)
	owner := f.status(t).Owner
	require.NotNil(t, owner)
	require.Equal(t, "resync", owner.Kind)
	awaitSourceLeaseExpiration(t, f, owner)
	return owner
}
