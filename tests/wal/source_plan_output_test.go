package wal_test

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWriteBackPlanOutput(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "candidate", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, branch.Name)
	require.NoError(t, err)
	ctx := t.Context()
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": int64(42), "i32": int32(1), "i64": int64(1), "dbl": float64(1)})
	require.NoError(t, err)
	cli := buildSourceCLI(t)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName,
			"ARGON_SOURCE_URI="+f.sourceURI, "ARGON_EXPERIMENTAL_SOURCE=1")
		body, err := cmd.Output()
		if failed, ok := err.(*exec.ExitError); ok {
			return failed.Stderr, err
		}
		return body, err
	}
	preview := []string{"merge", "preview", "-p", f.project, "-b", branch.Name}
	type change struct {
		ID       json.RawMessage            `json:"document_id"`
		Key      json.RawMessage            `json:"document_key"`
		Document map[string]json.RawMessage `json:"document"`
	}
	type plan struct {
		ID         string   `json:"id"`
		SourceHead int64    `json:"source_head"`
		Changes    []change `json:"changes"`
	}
	t.Run("numeric widths", func(t *testing.T) {
		body, err := run(append(preview, "-o", "json")...)
		require.NoError(t, err, "%s", body)
		var result struct{ Plan plan }
		require.NoError(t, json.Unmarshal(body, &result))
		require.NotEmpty(t, result.Plan.ID)
		require.Len(t, result.Plan.Changes, 1)
		doc := result.Plan.Changes[0]
		assert.JSONEq(t, `{"$numberLong":"42"}`, string(doc.ID))
		assert.JSONEq(t, `{"$numberLong":"42"}`, string(doc.Key))
		assert.JSONEq(t, `{"$numberLong":"42"}`, string(doc.Document["_id"]))
		assert.JSONEq(t, `{"$numberInt":"1"}`, string(doc.Document["i32"]))
		assert.JSONEq(t, `{"$numberLong":"1"}`, string(doc.Document["i64"]))
		assert.JSONEq(t, `{"$numberDouble":"1.0"}`, string(doc.Document["dbl"]))
	})
	t.Run("nonfinite values", func(t *testing.T) {
		before, err := f.services.Merge.ListPlans(ctx, main.ProjectID)
		require.NoError(t, err)
		_, err = writer.Put(ctx, "catalog", bson.M{"_id": "measurement", "value": math.NaN(), "lo": math.Inf(-1), "hi": math.Inf(1)})
		require.NoError(t, err)
		body, err := run(append(preview, "-o", "json")...)
		require.NoError(t, err, "%s", body)
		var result struct{ Plan plan }
		require.NoError(t, json.Unmarshal(body, &result))
		check := func(p plan) {
			t.Helper()
			require.Len(t, p.Changes, 2)
			for _, c := range p.Changes {
				if string(c.ID) == `"measurement"` {
					assert.JSONEq(t, `{"$numberDouble":"NaN"}`, string(c.Document["value"]))
					assert.JSONEq(t, `{"$numberDouble":"-Infinity"}`, string(c.Document["lo"]))
					assert.JSONEq(t, `{"$numberDouble":"Infinity"}`, string(c.Document["hi"]))
					return
				}
			}
			t.Fatal("missing measurement")
		}
		check(result.Plan)
		body, err = run("merge", "list", "-p", f.project, "-o", "json")
		require.NoError(t, err, "%s", body)
		var list struct{ Plans []plan }
		require.NoError(t, json.Unmarshal(body, &list))
		require.Len(t, list.Plans, len(before)+1)
		require.Equal(t, result.Plan.ID, list.Plans[0].ID)
		check(list.Plans[0])
	})
	t.Run("followed target", func(t *testing.T) {
		body, err := run(preview...)
		require.NoError(t, err, "%s", body)
		require.Contains(t, string(body), "ARGON_EXPERIMENTAL_SOURCE=1")
		require.Contains(t, string(body), "ARGON_SOURCE_URI")
		for line := range strings.SplitSeq(string(body), "\n") {
			if args, ok := strings.CutPrefix(line, "Apply with: argon "); ok {
				require.Contains(t, args, "--write-back")
				applied, err := run(strings.Fields(args)...)
				require.NoError(t, err, "%s", applied)
				return
			}
		}
		t.Fatal("missing apply command")
	})
	t.Run("output failure names saved plan", func(t *testing.T) {
		reader, sink, err := os.Pipe()
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		defer func() { require.NoError(t, sink.Close()) }()
		cmd := exec.CommandContext(ctx, cli, append(preview, "-o", "json")...)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
		cmd.Stdout = sink
		var stderr strings.Builder
		cmd.Stderr = &stderr
		require.Error(t, cmd.Run())
		plans, err := f.services.Merge.ListPlans(ctx, main.ProjectID)
		require.NoError(t, err)
		require.NotEmpty(t, plans)
		require.Contains(t, stderr.String(), plans[0].ID.Hex())
	})
	t.Run("damaged record", func(t *testing.T) {
		good := f.preview(t, branch.ID)
		id := primitive.NewObjectID()
		_, err := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans").InsertOne(ctx,
			bson.M{"_id": id, "project_id": main.ProjectID, "changes": "unreadable"})
		require.NoError(t, err)
		body, err := run("merge", "list", "-p", f.project, "-o", "json")
		require.NoError(t, err, "%s", body)
		var list struct {
			Plans []struct {
				ID     string
				Error  string
				Status string
			}
		}
		require.NoError(t, json.Unmarshal(body, &list))
		var damaged, healthy int
		for _, p := range list.Plans {
			if p.ID == id.Hex() {
				require.NotEmpty(t, p.Error)
				damaged++
			} else if p.ID == good.ID.Hex() && p.Status == "pending" {
				healthy++
			}
		}
		require.Equal(t, 1, damaged)
		require.Equal(t, 1, healthy)
	})
}
