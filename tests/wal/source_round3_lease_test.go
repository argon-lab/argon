//go:build unix

package wal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const testSourceLeasePeriod = 1500 * time.Millisecond
const testSourceHeartbeat = 100 * time.Millisecond

type followerProcessConfig struct {
	Metadata          string
	Project           string
	Source            string
	LeasePeriod       time.Duration
	HeartbeatInterval time.Duration
}

type followerProcessResult struct {
	Error         string
	Configuration bool
}

func TestFollow_FollowerProcess(t *testing.T) {
	path := os.Getenv("ARGON_TEST_FOLLOWER_CONFIG")
	if path == "" {
		t.Skip("helper subprocess")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg followerProcessConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	configureSourceLease(t, cfg.LeasePeriod, cfg.HeartbeatInterval)
	services, err := walcli.NewServicesAt(metadataDeploymentURI, cfg.Metadata)
	require.NoError(t, err)
	services.SynchronousSnapshots()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- services.FollowSource(ctx, cfg.Project, walcli.FollowSourceOptions{URI: cfg.Source, Ready: ready})
	}()
	select {
	case <-ready:
		require.NoError(t, os.WriteFile(path+".ready", nil, 0o600))
	case err := <-done:
		t.Fatalf("follower ended before opening its stream: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var disconnected bool
	for {
		select {
		case err := <-done:
			var result followerProcessResult
			if err != nil {
				var configuration *ingest.ConfigurationError
				result = followerProcessResult{Error: err.Error(), Configuration: errors.As(err, &configuration)}
			}
			data, err := json.Marshal(result)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path+".result.tmp", data, 0o600))
			require.NoError(t, os.Rename(path+".result.tmp", path+".result"))
			return
		case <-ticker.C:
			if _, err := os.Stat(path + ".disconnect"); err == nil && !disconnected {
				require.NoError(t, services.Client.Disconnect(ctx))
				disconnected = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func useShortSourceLease(t *testing.T) {
	t.Helper()
	configureSourceLease(t, testSourceLeasePeriod, testSourceHeartbeat)
}

func configureSourceLease(t *testing.T, period, interval time.Duration) {
	t.Helper()
	originalPeriod, originalInterval := ingest.SourceLeasePeriod, ingest.SourceHeartbeatInterval
	ingest.SourceLeasePeriod, ingest.SourceHeartbeatInterval = period, interval
	t.Cleanup(func() { ingest.SourceLeasePeriod, ingest.SourceHeartbeatInterval = originalPeriod, originalInterval })
}

func startFollowerProcess(t *testing.T, f *followFixture, path string) *exec.Cmd {
	t.Helper()
	data, err := json.Marshal(followerProcessConfig{
		Metadata: f.metadataName, Project: f.project, Source: f.sourceURI,
		LeasePeriod: ingest.SourceLeasePeriod, HeartbeatInterval: ingest.SourceHeartbeatInterval,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	cmd := exec.Command(os.Args[0], "-test.run", "^TestFollow_FollowerProcess$")
	cmd.Env = append(os.Environ(), "ARGON_TEST_FOLLOWER_CONFIG="+path)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() && output.Len() > 0 {
			t.Log(output.String())
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(path + ".ready")
		return err == nil
	}, 6*time.Second, 20*time.Millisecond)
	return cmd
}

func awaitFollowerProcessResult(t *testing.T, path string) followerProcessResult {
	t.Helper()
	var result followerProcessResult
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path + ".result")
		return err == nil && json.Unmarshal(data, &result) == nil
	}, 5*time.Second, 20*time.Millisecond, "follower did not stop on its own")
	return result
}

func awaitSourceLeaseExpiration(t *testing.T, f *followFixture, owner *walcli.SourceOwner) {
	t.Helper()
	require.NotNil(t, owner)
	require.Eventually(t, func() bool {
		var reply struct {
			LocalTime time.Time `bson:"localTime"`
		}
		err := f.services.Client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "hello", Value: 1}}).Decode(&reply)
		return err == nil && !reply.LocalTime.Before(owner.HeartbeatAt.Add(ingest.SourceLeasePeriod))
	}, 4*time.Second, 20*time.Millisecond)
}

func waitForFollowerHeartbeat(t *testing.T, f *followFixture, after time.Time) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := f.services.SourceStatus(context.Background(), f.project)
		return err == nil && status.Owner != nil && status.Owner.HeartbeatAt.After(after)
	}, 3*time.Second, 20*time.Millisecond)
}

func TestFollow_DeadFollowerReportsExpiredLeaseAndResumes(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	useShortSourceLease(t)

	cmd := startFollowerProcess(t, f, filepath.Join(t.TempDir(), "owner.json"))
	status := f.status(t)
	require.NotNil(t, status.Owner)
	require.Equal(t, cmd.Process.Pid, status.Owner.PID)
	require.Equal(t, "running", status.State)
	waitForFollowerHeartbeat(t, f, status.Owner.HeartbeatAt)
	require.NoError(t, cmd.Process.Kill())
	var killed *exec.ExitError
	require.ErrorAs(t, cmd.Wait(), &killed)
	last := f.status(t)
	awaitSourceLeaseExpiration(t, f, last.Owner)

	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	f.tick(t)
	info := f.status(t)
	require.True(t, info.LeaseExpired)
	require.Equal(t, "stopped", info.State)
	require.Equal(t, "exact", info.Mode)
	require.Contains(t, info.Error, "has not been heard from since")
	require.Contains(t, info.Error, last.Owner.Host)
	require.Equal(t, last.Owner, info.Owner)
	require.Equal(t, last.Position, info.Position)
	require.Equal(t, last.ConsistentLSN, info.ConsistentLSN)
	require.Equal(t, last.ConsistentTime, info.ConsistentTime)
	require.Equal(t, int32(10), f.mainState(t, 0)["catalog"]["item-00"]["stock"])

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	info = f.waitCaughtUp(t)
	require.False(t, info.LeaseExpired)
	require.Equal(t, os.Getpid(), info.Owner.PID)
	require.Equal(t, int32(99), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	require.NoError(t, stop())
}

func TestFollow_ExpiredOwnerCannotRenewWithoutSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	// Exercise the reader's custom period in the follower subprocess too.
	configureSourceLease(t, 750*time.Millisecond, testSourceHeartbeat)

	path := filepath.Join(t.TempDir(), "owner.json")
	cmd := startFollowerProcess(t, f, path)
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	last := f.status(t)
	awaitSourceLeaseExpiration(t, f, last.Owner)
	state := f.readIngestState(t, last.BranchID)
	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	f.tick(t)
	require.NoError(t, cmd.Process.Signal(syscall.SIGCONT))
	result := awaitFollowerProcessResult(t, path)
	require.True(t, result.Configuration)
	require.Contains(t, result.Error, "lease for branch main expired")
	require.Contains(t, result.Error, "run `argon source follow` again")
	require.Equal(t, state, f.readIngestState(t, last.BranchID), "the expired owner changed its checkpoint or report")
	require.Equal(t, last.HeadLSN, f.main(t).HeadLSN)
	require.Empty(t, f.followedEntries(t, last.HeadLSN))

	repair, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	// A replacement that stops during preflight leaves the lost owner visible.
	_, err = f.services.ResyncSource(repair, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI,
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, errors.New("the source replay stream is unavailable")
		},
	})
	require.ErrorContains(t, err, "the source replay stream is unavailable")
	infoWithoutOwner := f.status(t)
	require.Nil(t, infoWithoutOwner.Owner)
	require.Equal(t, "stopped", infoWithoutOwner.State)
	require.True(t, infoWithoutOwner.LeaseExpired)
	require.True(t, infoWithoutOwner.Unverified)
	require.Contains(t, infoWithoutOwner.Error, "last renewed its lease")
	require.Equal(t, last.Position, infoWithoutOwner.Position)

	// Force a full comparison even though the saved position can still resume.
	info, err := f.services.ResyncSource(repair, f.project, walcli.ResyncSourceOptions{
		URI: f.sourceURI, Force: true,
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			f.tick(t)
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		},
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.False(t, f.status(t).Unverified)
	require.Equal(t, int32(99), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
}

func TestFollow_RenewalOutageStopsTheFollower(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	useShortSourceLease(t)

	path := filepath.Join(t.TempDir(), "owner.json")
	startFollowerProcess(t, f, path)
	info := f.status(t)
	require.NotNil(t, info.Owner)
	waitForFollowerHeartbeat(t, f, info.Owner.HeartbeatAt)
	require.NoError(t, os.WriteFile(path+".disconnect", nil, 0o600))
	result := awaitFollowerProcessResult(t, path)
	require.True(t, result.Configuration)
	require.Contains(t, result.Error, "could not renew the source lease on the metadata deployment")
	require.Contains(t, result.Error, "run `argon source follow` again")

	// The observer still has its original connection to the metadata server.
	last := f.status(t)
	awaitSourceLeaseExpiration(t, f, last.Owner)
	info = f.status(t)
	require.Equal(t, "stopped", info.State)
	require.True(t, info.LeaseExpired)
	require.Contains(t, info.Error, "has not been heard from since")
	require.Equal(t, last.Owner, info.Owner)
}
