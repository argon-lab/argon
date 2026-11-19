package wal_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func runMergeCLI(t *testing.T, f *followFixture, cli string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), cli, args...)
	cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName,
		"ARGON_SOURCE_URI="+f.sourceURI, "ARGON_EXPERIMENTAL_SOURCE=1")
	body, err := cmd.Output()
	if failed, ok := err.(*exec.ExitError); ok {
		t.Logf("%s", failed.Stderr)
	}
	require.NoError(t, err)
	return body
}

func TestWriteBack_PlanImageJSON(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "catalog-edits", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, branch.Name)
	require.NoError(t, err)
	cli := buildSourceCLI(t)
	preview := []string{"merge", "preview", "-p", f.project, "-b", branch.Name}
	type change struct {
		Document json.RawMessage
		Paths    []string `json:"literal_dollar_paths"`
	}
	type plan struct{ Changes []change }

	output := func(t *testing.T) []plan {
		t.Helper()
		var saved struct{ Plan plan }
		require.NoError(t, json.Unmarshal(runMergeCLI(t, f, cli, append(preview, "-o", "json")...), &saved))
		var list struct{ Plans []plan }
		require.NoError(t, json.Unmarshal(runMergeCLI(t, f, cli, "merge", "list", "-p", f.project, "-o", "json"), &list))
		require.NotEmpty(t, list.Plans)
		return []plan{saved.Plan, list.Plans[0]}
	}
	t.Run("literal dollar paths", func(t *testing.T) {
		_, err := writer.Put(t.Context(), "catalog", bson.M{
			"_id": "sample", "quantity": int32(1), "annotation": bson.M{"$numberInt": "1"},
			"items": bson.A{bson.M{"$eq": "example", "$note": "same object"}, int64(2)},
		})
		require.NoError(t, err)
		for _, p := range output(t) {
			require.Len(t, p.Changes, 1)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(p.Changes[0].Document, &fields))
			require.JSONEq(t, `{"$numberInt":"1"}`, string(fields["quantity"]))
			require.JSONEq(t, string(fields["quantity"]), string(fields["annotation"]))
			require.ElementsMatch(t, []string{"document.annotation", "document.items.0"}, p.Changes[0].Paths)
		}
		table := runMergeCLI(t, f, cli, preview...)
		require.Contains(t, string(table), "2 image path(s) contain literal documents")
	})
	t.Run("DBPointer and legacy strings", func(t *testing.T) {
		const escaped = "\"\\\n\x02"
		pointer := primitive.DBPointer{DB: "catalog.shelf\"A", Pointer: primitive.NewObjectID()}
		key := "field" + escaped
		values := bson.D{
			{Key: "pointer" + escaped, Value: pointer},
			{Key: "symbol" + escaped, Value: primitive.Symbol(escaped)},
			{Key: "code" + escaped, Value: primitive.JavaScript(escaped)},
			{Key: "scope" + escaped, Value: primitive.CodeWithScope{Code: escaped, Scope: bson.D{
				{Key: key, Value: primitive.DBPointer{DB: escaped, Pointer: pointer.Pointer}},
			}}},
			{Key: "regex" + escaped, Value: primitive.Regex{Pattern: escaped, Options: "\x02\n\"\\"}},
			{Key: "binary" + escaped, Value: primitive.Binary{Subtype: 0x80, Data: []byte(escaped)}},
		}
		_, err := writer.Put(t.Context(), "catalog", bson.M{"_id": "sample", key: values})
		require.NoError(t, err)
		want, err := bson.Marshal(values)
		require.NoError(t, err)
		for _, p := range output(t) {
			require.Len(t, p.Changes, 1)
			c := p.Changes[0]
			require.Nil(t, c.Paths)
			var doc bson.Raw
			require.NoError(t, bson.UnmarshalExtJSON(c.Document, true, &doc))
			require.Equal(t, bson.Raw(want), doc.Lookup(key).Document())
		}
	})
}
