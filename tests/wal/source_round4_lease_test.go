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
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSource_ExpiredFollowerStaysUnhealthyDuringReplacementResyncPreflight(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	argon := buildSourceCLI(t)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	sandbox, sandboxDB := f.agentSandbox(t, "planned-edit")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-01", bson.M{"$set": bson.M{"stock": int32(100)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)

	follower := startFollowerProcess(t, f, filepath.Join(t.TempDir(), "owner.json"))
	last := f.status(t)
	require.NotNil(t, last.Owner)
	require.Equal(t, "running", last.State)
	require.NoError(t, follower.Process.Kill())
	require.Error(t, follower.Wait())
	metadata := f.services.Client.Database(f.metadataName)
	result, err := metadata.Collection("wal_ingest_state").UpdateByID(ctx, last.BranchID, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{"capture_status.owner.heartbeat_at": bson.M{"$subtract": bson.A{"$$NOW", int64(120_000)}}}}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)
	_, err = f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	expired := f.status(t)
	require.True(t, expired.LeaseExpired)
	require.Error(t, expired.HealthError())
	cliStatus := func(ctx context.Context) (walcli.SourceInfo, int, string) {
		cmd := exec.CommandContext(ctx, argon, "source", "status", "-p", f.project, "--output", "json")
		cmd.Env = append(os.Environ(), "ARGON_METADATA_DB="+f.metadataName, "MONGODB_URI="+metadataDeploymentURI)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		exit := 0
		if err != nil {
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", stderr.String())
			exit = exitErr.ExitCode()
		}
		var status walcli.SourceInfo
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &status), "%s", stderr.String())
		return status, exit, stderr.String()
	}
	_, expiredExit, _ := cliStatus(ctx)
	require.Equal(t, 1, expiredExit)
	_, expiredWriteBack := f.services.WriteBackMerge(ctx, plan.ID, "", "invalid-source-uri", approvalFor(t, plan))
	require.ErrorContains(t, expiredWriteBack, "last renewed its lease")

	resyncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	preflight := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan error, 1)
	defer func() {
		cancel()
		if finished != nil {
			<-finished
		}
	}()
	resultCh := finished
	probeFailure := errors.New("intentionally interrupted before recording resync")
	go func() {
		_, err := f.services.ResyncSource(resyncCtx, f.project, walcli.ResyncSourceOptions{
			URI: f.sourceURI,
			Watch: func(ctx context.Context, _ *mongo.Database, _ *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				close(preflight)
				select {
				case <-resume:
					return nil, probeFailure
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		})
		resultCh <- err
	}()
	select {
	case <-preflight:
	case err := <-finished:
		finished = nil
		t.Fatalf("resync returned before preflight: %v", err)
	case <-resyncCtx.Done():
		t.Fatal(resyncCtx.Err())
	}

	during := f.status(t)
	require.ErrorContains(t, during.HealthError(), "last renewed its lease")
	require.ErrorContains(t, during.HealthError(), "replacement is catching up")
	cliDuring, preflightExit, preflightStderr := cliStatus(resyncCtx)
	_, preflightWriteBack := f.services.WriteBackMerge(resyncCtx, plan.ID, "", "invalid-source-uri", approvalFor(t, plan))
	for _, args := range [][]string{{"source", "status", "-p", f.project}, {"status"}, {"doctor"}} {
		cmd := exec.CommandContext(resyncCtx, argon, args...)
		cmd.Env = append(os.Environ(), "ARGON_METADATA_DB="+f.metadataName, "MONGODB_URI="+metadataDeploymentURI)
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%v: %s", args, out)
		require.Equal(t, 1, exitErr.ExitCode(), "%v: %s", args, out)
		require.Contains(t, string(out), "replacement is catching up")
	}
	mainStock := f.mainState(t, 0)["catalog"]["item-00"]["stock"]
	var sourceDoc struct{ Stock int32 }
	err = f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-00"}).Decode(&sourceDoc)
	close(resume)
	resyncErr := <-finished
	finished = nil
	require.ErrorContains(t, resyncErr, probeFailure.Error())
	require.NoError(t, err)
	after := f.status(t)
	require.NotNil(t, during.Owner)
	require.Equal(t, "resync", during.Owner.Kind)
	require.False(t, during.ResyncUnfinished)
	require.Equal(t, int32(10), mainStock)
	require.Equal(t, int32(99), sourceDoc.Stock)
	require.Equal(t, last.HeadLSN, during.HeadLSN)
	require.Equal(t, last.Position, during.Position)
	require.Equal(t, last.ConsistentLSN, during.ConsistentLSN)
	require.Equal(t, last.ConsistentTime, during.ConsistentTime)
	require.Equal(t, "starting", during.State)
	require.Equal(t, during.State, cliDuring.State)
	require.True(t, cliDuring.Unverified)
	require.Contains(t, preflightStderr, "replacement is catching up")
	require.Equal(t, 1, preflightExit)
	require.ErrorContains(t, preflightWriteBack, "replacement is catching up")
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
	require.True(t, after.Unverified)
	require.Nil(t, after.Owner)
	require.ErrorContains(t, after.HealthError(), "Start `argon source follow`")
	_, err = f.services.ResyncSource(resyncCtx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, "`argon source follow` can resume from the saved position")
}
