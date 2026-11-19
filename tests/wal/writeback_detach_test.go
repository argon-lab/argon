package wal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
)

func detachPlan(t *testing.T) (*followFixture, *merge.Plan) {
	t.Helper()
	f, branch := planFixture(t)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.Put(t.Context(), "catalog", bson.M{"_id": "claim-test"})
	require.NoError(t, err)
	return f, f.preview(t, branch)
}

func TestWriteBack_DetachRacesClaim(t *testing.T) {
	for i := range 2 {
		for _, first := range []string{"detach", "claim"} {
			t.Run(fmt.Sprintf("%s-first-%d", first, i), func(t *testing.T) {
				f, plan := detachPlan(t)
				ctx := t.Context()
				var detached bool
				var detachErr error
				var snapshotRead int64
				meta := electionClient(t, metadataDeploymentURI, &event.CommandMonitor{
					Started: func(_ context.Context, e *event.CommandStartedEvent) {
						if e.CommandName == "aggregate" && e.Command.Lookup("autocommit").Type != 0 {
							snapshotRead = e.RequestID
						}
					},
					Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
						// With the snapshot open, detach forces the claim to retry
						// its conditional write. After claim acknowledgement, the
						// writer has yet to issue its first source command.
						atSnapshot := first == "detach" && e.CommandName == "aggregate" && e.RequestID == snapshotRead
						atClaim := first == "claim" && e.CommandName == "commitTransaction"
						if !detached && (atSnapshot || atClaim) {
							detached = true
							detachErr = f.services.DetachSource(ctx, f.project)
						}
					},
				})
				writer := merge.NewService(meta.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, meta)
				result, err := writer.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: f.source}, approvalFor(t, plan))
				require.True(t, detached)
				binding, readErr := f.services.Sources.Get(ctx, plan.TargetBranchID)
				require.NoError(t, readErr)
				docErr := f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "claim-test"}).Err()
				if first == "detach" {
					require.NoError(t, detachErr)
					require.ErrorContains(t, err, "following was detached")
					require.Nil(t, result)
					require.Nil(t, binding)
					require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
					require.ErrorIs(t, docErr, mongo.ErrNoDocuments)
				} else {
					require.ErrorContains(t, detachErr, plan.ID.Hex())
					require.ErrorContains(t, detachErr, "wait for the writer or for the follower to settle")
					require.NoError(t, err)
					require.Equal(t, 1, result.Applied)
					require.NotNil(t, binding)
					require.NotEmpty(t, f.main(t).Follows)
					require.NoError(t, docErr)
				}
			})
		}
	}
}

func TestSource_DetachReportsUnsettledPlans(t *testing.T) {
	cli := buildSourceCLI(t)
	for _, output := range []string{"table", "json"} {
		t.Run(output, func(t *testing.T) {
			f, plan := detachPlan(t)
			other := f.preview(t, plan.SourceBranchID)
			ctx := t.Context()
			plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
			for _, p := range []*merge.Plan{plan, other} {
				_, err := plans.UpdateByID(ctx, p.ID, bson.M{"$set": bson.M{"status": merge.StatusWritingBack}, "$currentDate": bson.M{"write_back.started_at": true}})
				require.NoError(t, err)
			}
			err := f.services.DetachSource(ctx, f.project)
			require.ErrorContains(t, err, plan.ID.Hex())
			require.ErrorContains(t, err, other.ID.Hex())
			require.NotEmpty(t, f.main(t).Follows)

			_, err = plans.UpdateMany(ctx, bson.M{"status": merge.StatusWritingBack}, mongo.Pipeline{{{Key: "$set", Value: bson.M{
				"write_back.started_at": bson.M{"$subtract": bson.A{"$$NOW", (6 * time.Minute).Milliseconds()}},
			}}}})
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, cli, "source", "detach", "-p", f.project, "--yes", "-o", output)
			cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
			out, err := cmd.Output()
			require.NoError(t, err, "%s", out)
			require.Empty(t, f.main(t).Follows)
			require.Contains(t, string(out), "inspect the source")
			if output == "json" {
				var result struct {
					Detached  bool     `json:"detached"`
					Unsettled []string `json:"unsettled_plans"`
					Warning   string   `json:"warning"`
				}
				require.NoError(t, json.Unmarshal(out, &result))
				require.True(t, result.Detached)
				require.Contains(t, result.Warning, "inspect the source")
				require.ElementsMatch(t, []string{plan.ID.Hex(), other.ID.Hex()}, result.Unsettled)
			}
			for _, p := range []*merge.Plan{plan, other} {
				require.Contains(t, string(out), p.ID.Hex())
				require.Equal(t, merge.StatusWritingBack, f.plan(t, p).Status)
				_, err := f.services.WriteBackMerge(ctx, p.ID, "", f.sourceURI, approvalFor(t, p))
				require.ErrorContains(t, err, "will not be written again")
			}
		})
	}
}

func TestWriteBack_DeadlineCoversCommit(t *testing.T) {
	f, plan := detachPlan(t)
	var reachedCommit, hasDeadline bool
	var maxTime int64
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	src := electionClient(t, f.sourceURI, &event.CommandMonitor{Started: func(commandCtx context.Context, e *event.CommandStartedEvent) {
		if e.CommandName != "commitTransaction" {
			return
		}
		reachedCommit = true
		_, hasDeadline = commandCtx.Deadline()
		maxTime, _ = e.Command.Lookup("maxTimeMS").AsInt64OK()
		// Command monitoring runs immediately before the driver's socket write.
		// Let the deadline pass with the complete transaction still uncommitted.
		<-ctx.Done()
	}})
	result, err := f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: src.Database(f.source.Name())}, approvalFor(t, plan))
	require.True(t, reachedCommit)
	require.True(t, hasDeadline)
	require.Positive(t, maxTime)
	require.LessOrEqual(t, maxTime, (2 * time.Minute).Milliseconds())
	require.Nil(t, result)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, f.source.Collection("catalog").FindOne(t.Context(), bson.M{"_id": "claim-test"}).Err(), mongo.ErrNoDocuments)
}

func TestWriteBack_DetachBeforeClaim(t *testing.T) {
	f, plan := detachPlan(t)
	ctx := t.Context()
	var detached bool
	var detachErr error
	src := electionClient(t, f.sourceURI, &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		// Source preflight follows the first admission and precedes the claim.
		if e.CommandName == "listCollections" && !detached {
			detached = true
			detachErr = f.services.DetachSource(ctx, f.project)
		}
	}})
	result, err := f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: src.Database(f.source.Name())}, approvalFor(t, plan))
	require.True(t, detached)
	require.NoError(t, detachErr)
	require.ErrorContains(t, err, "following was detached")
	require.Nil(t, result)
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
	require.ErrorIs(t, f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "claim-test"}).Err(), mongo.ErrNoDocuments)
}

func TestWriteBack_DeadlineStartsBeforeClaim(t *testing.T) {
	f, plan := detachPlan(t)
	ctx := context.WithoutCancel(t.Context())
	var claimDeadline, commitDeadline time.Time
	meta := electionClient(t, metadataDeploymentURI, &event.CommandMonitor{Started: func(sc context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "update" && e.Command.Lookup("autocommit").Type != 0 && e.Command.Lookup("update").StringValue() == "wal_merge_plans" {
			claimDeadline, _ = sc.Deadline()
		}
	}})
	src := electionClient(t, f.sourceURI, &event.CommandMonitor{Started: func(sc context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "commitTransaction" {
			commitDeadline, _ = sc.Deadline()
		}
	}})
	writer := merge.NewService(meta.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, meta)
	start := time.Now()
	_, err := writer.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: src.Database(f.source.Name())}, approvalFor(t, plan))
	require.NoError(t, err)
	require.False(t, claimDeadline.IsZero())
	require.Equal(t, claimDeadline, commitDeadline)
	require.WithinRange(t, claimDeadline, start.Add(2*time.Minute), time.Now().Add(2*time.Minute))
}
