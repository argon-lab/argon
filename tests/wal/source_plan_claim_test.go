package wal_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

func planFixture(t *testing.T) (*followFixture, string) {
	t.Helper()
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "edits", main.ID)
	require.NoError(t, err)
	return f, branch.ID
}

func TestWriteBack_EmptyPlan(t *testing.T) {
	f, branchID := planFixture(t)
	plan := f.preview(t, branchID)
	// Check the source on its own deployment, independent of metadata commits.
	commits := func() int64 {
		t.Helper()
		raw, err := f.sourceClient.Database("admin").RunCommand(t.Context(), bson.D{{Key: "serverStatus", Value: 1}}).Raw()
		require.NoError(t, err)
		return raw.Lookup("transactions", "totalCommitted").AsInt64()
	}
	before := commits()
	cmd := exec.CommandContext(t.Context(), buildSourceCLI(t), "merge", "apply", plan.ID.Hex(), "--write-back", "--uri", f.sourceURI, "--output", "json")
	cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
	body, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok {
		t.Logf("merge apply: %s", exit.Stderr)
	}
	require.NoError(t, err)
	var output struct {
		Written *bool `json:"written_to_source"`
	}
	require.NoError(t, json.Unmarshal(body, &output))
	require.NotNil(t, output.Written)
	require.False(t, *output.Written)
	require.Equal(t, before, commits())
	done := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, done.Status)
	require.NotNil(t, done.AppliedAt)
	require.Nil(t, done.WriteBack)
}

func TestWriteBack_RetryClearsFailure(t *testing.T) {
	f, branchID := planFixture(t)
	ctx := t.Context()
	col := f.source.Collection("catalog")
	_, err := col.UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"tag": "reserved"}})
	require.NoError(t, err)
	_, err = col.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "tag", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)})
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "new-item", "tag": "reserved"})
	require.NoError(t, err)
	plan := f.preview(t, branchID)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.True(t, mongo.IsDuplicateKeyError(err), "%v", err)
	require.Contains(t, f.plan(t, plan).WriteBackLastError, "duplicate key")
	_, err = col.UpdateByID(ctx, "item-00", bson.M{"$unset": bson.M{"tag": ""}})
	require.NoError(t, err)
	var claimed *merge.Plan
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "insert" {
			claimed = f.plan(t, plan)
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	result, err := f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: client.Database(f.source.Name())})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, merge.StatusWritingBack, claimed.Status)
	require.Empty(t, claimed.WriteBackLastError)
	require.Equal(t, 1, result.Applied)
	done := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, done.Status)
	require.Empty(t, done.WriteBackLastError)
}

func TestWriteBack_OversizedPreview(t *testing.T) {
	f, branchID := planFixture(t)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	payload := strings.Repeat("x", 1<<20)
	for i := range 17 {
		_, err = writer.Put(t.Context(), "catalog", bson.M{"_id": i, "payload": payload})
		require.NoError(t, err)
	}
	_, err = f.services.Merge.Preview(t.Context(), branchID)
	require.ErrorContains(t, err, "16 MiB limit (16777216 bytes)")
	require.Regexp(t, `encoded merge plan is [0-9]+ bytes`, err.Error())
	require.Contains(t, err.Error(), "smaller for one reviewed plan")
	plans, err := f.services.Merge.ListPlans(t.Context(), f.main(t).ProjectID)
	require.NoError(t, err)
	require.Empty(t, plans)
}

func TestWriteBack_StrategyPrecedesPreflight(t *testing.T) {
	f, branchID := planFixture(t)
	plan := f.preview(t, branchID)
	for _, id := range []primitive.ObjectID{plan.ID, primitive.NewObjectID()} {
		_, err := f.services.Merge.Apply(t.Context(), id, "typo")
		require.ErrorContains(t, err, "unknown strategy")
		_, err = f.services.Merge.ApplyToSource(t.Context(), id, "typo", merge.WriteBackTarget{})
		require.ErrorContains(t, err, "unknown strategy")
		_, err = f.services.WriteBackMerge(t.Context(), id, "typo", "invalid URI")
		require.ErrorContains(t, err, "unknown strategy")
	}
	done := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, done.Status)
	require.Empty(t, done.Strategy)
}

func TestWriteBack_CheckpointChangesBeforeClaim(t *testing.T) {
	f, _ := planFixture(t)
	plan := f.pendingSourceEdit(t, t.Context())
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	before := f.sourceState(t, nil)
	var changed bool
	// Source preparation starts after the initial capture check. Remove its
	// checkpoint before the metadata claim takes its own snapshot.
	monitor := &event.CommandMonitor{Succeeded: func(ctx context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "listCollections" {
			result, err := state.UpdateByID(ctx, plan.TargetBranchID, bson.M{"$unset": bson.M{"start_at": "", "resume_token": ""}})
			require.NoError(t, err)
			require.EqualValues(t, 1, result.ModifiedCount)
			changed = true
		}
	}}
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	_, err = f.services.Merge.ApplyToSource(t.Context(), plan.ID, "", merge.WriteBackTarget{Database: client.Database(f.source.Name())})
	require.True(t, changed)
	require.ErrorContains(t, err, "write-back refused")
	done := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, done.Status)
	require.Nil(t, done.WriteBack)
	require.Equal(t, before, f.sourceState(t, nil))
}

func TestFollow_ClaimLookupReadsPrimary(t *testing.T) {
	f, branchID := planFixture(t)
	plan := f.preview(t, branchID)
	session := primitive.Binary{Subtype: 4, Data: make([]byte, 16)}
	_, err := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans").UpdateByID(t.Context(), plan.ID,
		bson.M{"$set": bson.M{"write_back.session": session}})
	require.NoError(t, err)
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(metadataDeploymentURI).SetReadPreference(readpref.Secondary()).SetServerSelectionTimeout(2*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	reader := merge.NewService(client.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, client)
	claim, err := reader.LookupWriteBack(t.Context(), plan.TargetBranchID, session)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, plan.ID, claim.ID)
}
