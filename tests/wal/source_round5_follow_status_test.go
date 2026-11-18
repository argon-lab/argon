package wal_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (f *followFixture) sourceStatusCLI(t *testing.T, ctx context.Context, cli string) (walcli.SourceInfo, int) {
	t.Helper()
	cmd := exec.CommandContext(ctx, cli, "source", "status", "-p", f.project, "--output", "json")
	cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
	}
	var info walcli.SourceInfo
	require.NoError(t, json.Unmarshal(out, &info), "%s", out)
	return info, cmd.ProcessState.ExitCode()
}

func (f *followFixture) deploymentCaptureHealthy(t *testing.T, ctx context.Context) bool {
	t.Helper()
	reports, healthy, err := walcli.InspectCaptureHealth(ctx, f.services.Client.Database(f.metadataName))
	require.NoError(t, err)
	mainID := f.main(t).ID
	require.True(t, slices.ContainsFunc(reports, func(report bson.M) bool {
		return report["branch_id"] == mainID
	}), "deployment report must include main")
	return healthy
}

func (f *followFixture) pendingSourceEdit(t *testing.T, ctx context.Context) *merge.Plan {
	t.Helper()
	sandbox, db := f.agentSandbox(t, "proposed")
	_, err := db.Collection("catalog").UpdateByID(ctx, "item-01", bson.M{"$set": bson.M{"stock": int32(50)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)
	return plan
}

// Both entry points must report the saved reason before opening the source
// or claiming the pending plan.
func (f *followFixture) requireWriteBackRefused(t *testing.T, ctx context.Context, plan *merge.Plan, detail string) {
	t.Helper()
	_, err := f.services.WriteBackMerge(ctx, plan.ID, "", "invalid-source-uri")
	require.ErrorContains(t, err, detail)
	_, err = f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{})
	require.ErrorContains(t, err, detail)
	saved := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, saved.Status)
	require.Nil(t, saved.WriteBack)
}

func TestSource_TerminalStreamErrorIsUnhealthyAfterStop(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	cli := buildSourceCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.NoError(t, stop())
	clean, exit := f.sourceStatusCLI(t, ctx, cli)
	require.Equal(t, "stopped", clean.State)
	require.Empty(t, clean.Error)
	require.Equal(t, ingest.ReasonExact, clean.Health.Reason)
	require.Zero(t, exit)
	require.True(t, f.deploymentCaptureHealthy(t, ctx))
	plan := f.pendingSourceEdit(t, ctx)

	_, err := f.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	err = f.followUntilItStops(t, walcli.FollowSourceOptions{
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 40415, Message: "unrecognized change-stream option"}
		},
	})
	require.ErrorContains(t, err, "does not support the change-stream options")
	failed, exit := f.sourceStatusCLI(t, ctx, cli)
	require.Equal(t, "stopped", failed.State)
	require.Contains(t, failed.Error, "does not support the change-stream options")
	require.Equal(t, clean.Position, failed.Position)
	require.Equal(t, int32(10), f.mainState(t, 0)["catalog"]["item-00"]["stock"])
	require.Equal(t, int32(99), f.sourceState(t, nil)["catalog"]["item-00"]["stock"])
	require.Equal(t, ingest.ReasonCaptureError, failed.Health.Reason)
	require.Equal(t, 1, exit)
	require.ErrorContains(t, failed.HealthError(), "unrecognized change-stream option")
	require.False(t, f.deploymentCaptureHealthy(t, ctx))
	f.requireWriteBackRefused(t, ctx, plan, failed.Health.Message)
}

func TestFollow_CanceledOpenRetainsCaptureError(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	const terminal = "unrecognized change-stream option"
	err := f.followUntilItStops(t, walcli.FollowSourceOptions{
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 40415, Message: terminal}
		},
	})
	require.ErrorContains(t, err, terminal)

	for _, retry := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		detail := terminal
		if retry {
			detail = "network outage"
		}
		opened := 0
		err = f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI,
			Watch: func(ctx context.Context, _ *mongo.Database, _ *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				opened++
				if retry && opened == 1 {
					return nil, mongo.CommandError{Message: detail, Labels: []string{"NetworkError"}}
				}
				require.ErrorContains(t, f.status(t).HealthError(), detail)
				cancel()
				return nil, ctx.Err()
			},
		})
		cancel()
		require.NoError(t, err)
		stopped := f.status(t)
		require.Equal(t, "stopped", stopped.State)
		require.Nil(t, stopped.Owner)
		require.Equal(t, ingest.ReasonCaptureError, stopped.Health.Reason)
		require.ErrorContains(t, stopped.HealthError(), detail)
	}

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	require.NoError(t, stop())
	clean := f.status(t)
	require.Equal(t, "stopped", clean.State)
	require.Empty(t, clean.Error)
	require.NoError(t, clean.HealthError())
}
