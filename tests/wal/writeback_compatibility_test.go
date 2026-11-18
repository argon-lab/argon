package wal_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func v270Fixture(t *testing.T, name string) bson.Raw {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v2.7.0", name+".json"))
	require.NoError(t, err)
	var raw bson.Raw
	require.NoError(t, bson.UnmarshalExtJSON(data, true, &raw))
	return raw
}

// The plan and its branch/WAL records keep their original IDs. Only the
// followed database's location and checkpoint come from this replica set.
func bindFixtureSource(t *testing.T, f *followFixture, plan *merge.Plan) {
	t.Helper()
	ctx := t.Context()
	_, err := f.source.Collection("catalog").InsertOne(ctx, bson.D{{Key: "_id", Value: "baseline"}, {Key: "value", Value: int32(0)}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	require.Equal(t, plan.TargetHead, main.HeadLSN)
	binding, err := f.services.Sources.Get(ctx, main.ID)
	require.NoError(t, err)
	binding.BranchID, binding.ProjectID = plan.TargetBranchID, plan.ProjectID
	require.NoError(t, f.services.Sources.Bind(ctx, binding))
	require.NoError(t, f.services.Branches.SetFollows(ctx, plan.TargetBranchID, f.source.Name()))

	// Copy the checkpoint provided by the matching one-document import
	// without decoding the resume token into an unordered map.
	checkpoints := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	var checkpoint bson.D
	require.NoError(t, checkpoints.FindOne(ctx, bson.M{"_id": main.ID}).Decode(&checkpoint))
	for i := range checkpoint {
		if checkpoint[i].Key == "_id" {
			checkpoint[i].Value = plan.TargetBranchID
		}
	}
	_, err = checkpoints.InsertOne(ctx, checkpoint)
	require.NoError(t, err)
}

func TestWriteBack_PlansFromV270(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	db := f.services.Client.Database(f.metadataName)
	cli := buildSourceCLI(t)
	run := func(t *testing.T, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), cli, args...)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
		out, err := cmd.Output()
		if exit, ok := err.(*exec.ExitError); ok {
			t.Logf("argon %v: %s", args, exit.Stderr)
		}
		require.NoError(t, err)
		return out
	}

	// The four pending plans were saved by the v2.7.0 CLI. Its WriterFor
	// supplied the editable branches. Restore the compressed WAL as stored.
	var state map[string][]bson.Raw
	require.NoError(t, bson.Unmarshal(v270Fixture(t, "state"), &state))
	for name, rows := range state {
		require.NotEmpty(t, rows)
		for _, row := range rows {
			_, err := db.Collection(name).InsertOne(t.Context(), row)
			require.NoError(t, err)
		}
	}
	for _, kind := range []string{"conflict", "compound", "nested", "followed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			raw := v270Fixture(t, kind)
			id := raw.Lookup("_id").ObjectID()
			_, err := db.Collection("wal_merge_plans").InsertOne(ctx, raw)
			require.NoError(t, err)
			plan, err := f.services.Merge.GetPlan(ctx, id)
			require.NoError(t, err)
			require.Equal(t, merge.StatusPending, plan.Status)
			list := []string{"merge", "list", "-p", "v270-" + kind}
			require.Contains(t, string(run(t, list...)), id.Hex())
			var listed struct{ Plans []struct{ ID string } }
			require.NoError(t, json.Unmarshal(run(t, append(list, "-o", "json")...), &listed))
			require.Len(t, listed.Plans, 1)
			require.Equal(t, id.Hex(), listed.Plans[0].ID)

			var oldNested bson.RawValue
			if kind == "nested" || kind == "followed" {
				oldNested = fixtureEntryImage(t, f, plan.SourceBranchID).Lookup("nested")
			}
			apply := []string{"merge", "apply", id.Hex(), "-o", "json"}
			switch kind {
			case "conflict":
				require.Len(t, plan.Conflicts, 1)
				apply = append(apply, "--strategy", "theirs")
			case "followed":
				bindFixtureSource(t, f, plan)
				apply = append(apply, "--write-back", "--uri", f.sourceURI)
			}
			var result struct{ Applied int }
			require.NoError(t, json.Unmarshal(run(t, apply...), &result))
			require.Equal(t, 1, result.Applied)
			require.Equal(t, merge.StatusApplied, f.plan(t, plan).Status)
			if kind == "followed" {
				require.Equal(t, oldNested, fixtureEntryImage(t, f, plan.SourceBranchID).Lookup("nested"))
				got, err := f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
				require.NoError(t, err)
				require.Equal(t, oldNested, got.Lookup("nested"), "the existing WAL image keeps its field order")
				return
			}
			history, err := f.services.WAL.GetEntries(bson.M{"txn_id": "merge:" + id.Hex(), "operation": wal.OpPut})
			require.NoError(t, err)
			require.Len(t, history, 1)
			image := history[0].PostImage
			switch kind {
			case "conflict":
				require.Equal(t, int32(3), image.Lookup("value").Int32())
			case "compound":
				key, err := wal.DocumentIDFromImage(image)
				require.NoError(t, err)
				require.Equal(t, bson.D{{Key: "zone", Value: "east"}, {Key: "bay", Value: int32(12)}}, key)
				require.Equal(t, int64(7), image.Lookup("value").Int64())
			case "nested":
				require.Equal(t, oldNested, image.Lookup("nested"))
				require.Equal(t, int32(12), image.Lookup("nested", "bay").Int32())
				require.Equal(t, int64(17), image.Lookup("nested", "shelf").Int64())
				require.Equal(t, int64(2), image.Lookup("nested", "boxes", "0", "units").Int64())
			}
		})
	}
}

func fixtureEntryImage(t *testing.T, f *followFixture, id string) bson.Raw {
	t.Helper()
	entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": id, "operation": wal.OpPut})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	return entries[0].PostImage
}
