package wal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func buildSourceCLI(t *testing.T) string {
	t.Helper()
	cli := filepath.Join(t.TempDir(), "argon")
	build := exec.Command("go", "build", "-o", cli, ".")
	build.Dir = "../../cli"
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return cli
}

func TestSource_StatusReportsRetryUntilTheStreamReopens(t *testing.T) {
	uri := reproSourceURI(t)
	cli := buildSourceCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFollowFixture(t, uri)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	lastGood := f.status(t)
	require.NoError(t, lastGood.HealthError())
	require.Nil(t, lastGood.Owner)

	source := f.source.Collection("catalog")
	_, err := source.UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	var attempts int
	retryStarted := make(chan struct{})
	resumeRetry := make(chan struct{})
	followerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- f.services.FollowSource(followerCtx, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI,
			Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				attempts++
				switch attempts {
				case 1, 2:
					return nil, mongo.CommandError{Message: "test source network outage", Labels: []string{"NetworkError"}}
				case 3:
					close(retryStarted)
					select {
					case <-resumeRetry:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return src.Watch(ctx, mongo.Pipeline{}, opts)
			},
		})
	}()
	defer func() {
		stop()
		<-done
	}()

	select {
	case <-retryStarted:
	case <-ctx.Done():
		t.Fatal("follower did not retry opening the source stream")
	}
	status := f.status(t)
	require.Equal(t, "retrying", status.State)
	require.NotNil(t, status.Owner)
	require.False(t, status.LeaseExpired)
	require.Contains(t, status.Error, "test source network outage")
	require.Equal(t, lastGood.Position, status.Position)
	require.Equal(t, lastGood.ConsistentLSN, status.ConsistentLSN)
	require.Equal(t, lastGood.ConsistentTime, status.ConsistentTime)
	mainStock := f.mainState(t, 0)["catalog"]["item-00"]["stock"]
	sourceStock := f.sourceState(t, nil)["catalog"]["item-00"]["stock"]
	require.Equal(t, int32(10), mainStock)
	require.Equal(t, int32(99), sourceStock)

	require.ErrorContains(t, status.HealthError(), "test source network outage")
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			command := exec.CommandContext(ctx, cli, "source", "status", "-p", f.project, "--output", format)
			command.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
			output, cliErr := command.Output()
			var exit *exec.ExitError
			require.ErrorAs(t, cliErr, &exit, "source status accepted a follower that cannot reopen its stream")
			require.Equal(t, 1, exit.ExitCode())
			require.Contains(t, string(exit.Stderr), "currently unable to read it")
			require.Contains(t, string(output), "test source network outage")
			if format == "json" {
				var info walcli.SourceInfo
				require.NoError(t, json.Unmarshal(output, &info))
				require.Equal(t, "retrying", info.State)
				require.Equal(t, lastGood.ConsistentLSN, info.ConsistentLSN)
				require.Equal(t, lastGood.ConsistentTime, info.ConsistentTime)
			} else {
				require.Contains(t, string(output), "State:       retrying")
				require.Contains(t, string(output), "Consistent:  LSN "+fmt.Sprint(lastGood.ConsistentLSN))
			}
		})
	}
	metadata := f.services.Client.Database(f.metadataName)
	reports, healthy, err := walcli.InspectCaptureHealth(ctx, metadata)
	require.NoError(t, err)
	require.False(t, healthy)
	require.Len(t, reports, 1)
	require.Equal(t, "retrying", reports[0]["state"])
	require.Contains(t, reports[0]["error"], "test source network outage")

	for _, name := range []string{"status", "doctor"} {
		cmd := exec.CommandContext(ctx, cli, name)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "%s", out)
		require.Equal(t, 1, exit.ExitCode(), "%s", out)
		require.Contains(t, string(out), "test source network outage")
	}

	close(resumeRetry)
	recovered := f.waitCaughtUp(t)
	require.Equal(t, "running", recovered.State)
	require.Empty(t, recovered.Error)
	require.NoError(t, recovered.HealthError())
	require.Equal(t, int32(99), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	_, healthy, err = walcli.InspectCaptureHealth(ctx, metadata)
	require.NoError(t, err)
	require.True(t, healthy)
}
