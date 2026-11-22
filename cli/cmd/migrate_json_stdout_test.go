package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// With -o json, stdout is exactly one JSON value; the progress line goes to stderr.
func TestMigrateWALJSONIsTheOnlyStdout(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB integration workflow")
	}
	t.Setenv("ARGON_METADATA_DB", "argon_cli_test_"+primitive.NewObjectID().Hex())
	project := "migrate-" + primitive.NewObjectID().Hex()
	if stdout, stderr, err := invokeCLI(t, uri, "projects", "create", project); err != nil {
		t.Fatalf("projects create: %v %s %s", err, stdout, stderr)
	}
	stdout, stderr, err := invokeCLI(t, uri, "migrate-wal", "-p", project, "-o", "json")
	if err != nil {
		t.Fatalf("migrate-wal: %v %s %s", err, stdout, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not one JSON value: %q (%v)", stdout, err)
	}
	if report["project"] != project {
		t.Fatalf("project = %v, want %s", report["project"], project)
	}
	if !strings.Contains(stderr, "Migrating WAL for project") {
		t.Fatalf("progress line missing from stderr: %q", stderr)
	}
}
