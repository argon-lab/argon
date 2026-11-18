//go:build unix

package wal_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSource_LeaseExpiryOnHeldSnapshot(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	useShortSourceLease(t)
	cmd := startFollowerProcess(t, f, filepath.Join(t.TempDir(), "owner.json"))
	branchID := f.main(t).ID
	db := f.services.Client.Database(f.metadataName)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := f.services.Client.StartSession(options.Session().SetSnapshot(true))
	require.NoError(t, err)
	defer session.EndSession(ctx)
	held := mongo.NewSessionContext(ctx, session)
	read := func(readCtx context.Context) *ingest.Status {
		t.Helper()
		_, status, err := ingest.ReadCaptureStatus(readCtx, db, branchID)
		require.NoError(t, err)
		require.NotNil(t, status.Owner)
		return status
	}
	initial := read(held)
	require.Equal(t, ingest.ReasonExact, initial.Health.Reason)
	deadline := initial.Owner.HeartbeatAt.Add(ingest.SourceLeasePeriod)
	_, err = f.services.WAL.WithTransaction(ctx, func(tx mongo.SessionContext) (interface{}, error) {
		pinned := read(tx)
		require.Eventually(t, func() bool {
			return read(ctx).Owner.HeartbeatAt.After(pinned.Owner.HeartbeatAt.Add(ingest.SourceLeasePeriod))
		}, 6*time.Second, 100*time.Millisecond)
		// A takeover transaction keeps the heartbeat it will fence.
		require.Equal(t, pinned.Owner.HeartbeatAt, read(tx).Owner.HeartbeatAt)
		return nil, nil
	})
	require.NoError(t, err)

	renewed := read(held)
	require.Equal(t, ingest.ReasonExact, renewed.Health.Reason)
	require.Equal(t, initial.Owner.ID, renewed.Owner.ID)
	require.True(t, renewed.Owner.HeartbeatAt.After(deadline))
	reports, err := ingest.ReadCaptureReports(held, db)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.True(t, reports[0].Healthy)
	require.Equal(t, ingest.ReasonExact, reports[0].Status.Health.Reason)
	require.True(t, reports[0].Status.Owner.HeartbeatAt.After(deadline))

	require.NoError(t, cmd.Process.Kill())
	var killed *exec.ExitError
	require.ErrorAs(t, cmd.Wait(), &killed)
	last := f.status(t).Owner
	awaitSourceLeaseExpiration(t, f, last)
	for _, readCtx := range []context.Context{ctx, held} {
		expired := read(readCtx)
		require.Equal(t, ingest.ReasonOwnerExpired, expired.Health.Reason)
		require.Equal(t, last.HeartbeatAt, expired.Owner.HeartbeatAt)
		reports, err := ingest.ReadCaptureReports(readCtx, db)
		require.NoError(t, err)
		require.Len(t, reports, 1)
		require.False(t, reports[0].Healthy)
		require.Equal(t, ingest.ReasonOwnerExpired, reports[0].Status.Health.Reason)
		require.Equal(t, last.HeartbeatAt, reports[0].Status.Owner.HeartbeatAt)
	}
}
