package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("ARGON_CLI_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			rootCmd.SetArgs(os.Args[i+1:])
			break
		}
	}
	if err := Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func invokeCLI(t *testing.T, uri string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIHelperProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1", "MONGODB_URI="+uri)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func TestCLIDocumentedCommandsAndUnsupportedFlags(t *testing.T) {
	stdout, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1", "--version")
	if err != nil || !strings.Contains(stdout, rootCmd.Version) {
		t.Fatalf("version mismatch: %q %v %s", stdout, err, stderr)
	}
	for _, args := range [][]string{
		{"collections", "prepare", "--help"}, {"doctor", "--help"}, {"watch", "--help"}, {"console", "--help"},
		{"projects", "list", "--help"}, {"branches", "create", "--help"},
		{"sandbox", "create", "--help"}, {"pin", "sandbox", "--help"},
		{"merge", "preview", "--help"}, {"undo", "--help"}, {"time-travel", "query", "--help"},
	} {
		_, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1", args...)
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, stderr)
		}
	}
	for _, args := range [][]string{{"projects", "list", "--output", "yaml"}, {"projects", "list", "--api-key", "unused"}, {"watch", "--output", "json"}, {"time-travel", "info", "--time", "1h ago"}} {
		_, _, err := invokeCLI(t, "mongodb://127.0.0.1:1", args...)
		if err == nil {
			t.Fatalf("unsupported command accepted: %v", args)
		}
	}
}

func TestCLIStatusConnectionFailureIsJSONAndNonzero(t *testing.T) {
	stdout, _, err := invokeCLI(t, "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100", "status", "--output", "json")
	if err == nil {
		t.Fatal("unreachable MongoDB must fail")
	}
	var report deploymentReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout, err)
	}
	if report.Healthy {
		t.Fatal("unreachable MongoDB reported healthy")
	}
}

func TestCLIPinnedWorkflowJSON(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB integration workflow")
	}
	t.Setenv("ARGON_METADATA_DB", "argon_cli_test_"+primitive.NewObjectID().Hex())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	assertCaptureStopped := func() {
		t.Helper()
		count, err := client.Database(metadataDatabase()).Collection("wal_ingest_state").CountDocuments(ctx,
			bson.M{"capture_status.state": bson.M{"$exists": true, "$nin": bson.A{"stopped", "degraded"}}})
		if err != nil || count != 0 {
			t.Fatalf("command exited with active capture: count=%d error=%v", count, err)
		}
	}
	call := func(args ...string) map[string]any {
		t.Helper()
		stdout, stderr, err := invokeCLI(t, uri, append(args, "--output", "json")...)
		if err != nil {
			for i, arg := range args {
				if arg == "-p" && i+1 < len(args) {
					var project bson.M
					_ = client.Database(metadataDatabase()).Collection("wal_projects").FindOne(ctx, bson.M{"name": args[i+1]}).Decode(&project)
					var status bson.M
					_ = client.Database(metadataDatabase()).Collection("wal_ingest_state").FindOne(ctx, bson.M{"_id": project["main_branch_id"]}).Decode(&status)
					t.Logf("main capture after failure: %v", status["capture_status"])
				}
			}
			t.Fatalf("%v failed: %v %s %s", args, err, stdout, stderr)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("%v output is not one JSON value: %q (%v)", args, stdout, err)
		}
		assertCaptureStopped()
		return result
	}
	call("doctor")
	project := "cli-test-" + primitive.NewObjectID().Hex()
	created := call("projects", "create", project)["project"].(map[string]any)
	if created["id"] == "" {
		t.Fatal("project ID missing")
	}
	var databases []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range databases {
			_ = client.Database(name).Drop(cleanup)
		}
		_ = client.Database(metadataDatabase()).Drop(cleanup)
	}()
	call("projects", "list")
	checkout := call("checkout", "-p", project)
	dbName := checkout["physical_db"].(string)
	databases = append(databases, dbName)
	if checkout["capture_managed"] != false {
		t.Fatal("one-shot checkout must not claim a managed watcher")
	}
	call("collections", "prepare", "accounts", "-p", project)
	if _, err := client.Database(dbName).Collection("accounts").InsertOne(ctx, bson.M{"_id": "customer", "discount": 0}); err != nil {
		t.Fatal(err)
	}
	// Pin syncs even when no separate watch process was started.
	pin := call("pin", "create", "-p", project, "--name", "baseline")["pin"].(map[string]any)
	if pin["lsn"].(float64) <= 0 {
		t.Fatal("pin did not capture seeded data")
	}
	// Duplicate pin creation fails after synchronization has started capture.
	// Error returns must also join its checkpoint worker before process exit.
	if _, _, err := invokeCLI(t, uri, "pin", "create", "-p", project, "--name", "baseline", "--output", "json"); err == nil {
		t.Fatal("duplicate pin must fail")
	}
	assertCaptureStopped()
	call("pin", "list", "-p", project)
	sandbox := call("pin", "sandbox", "-p", project, "--name", "baseline", "--as", "candidate")
	uriValue := sandbox["connection_string"].(string)
	physical := strings.Split(strings.Split(uriValue, "://")[1], "/")[1]
	physical = strings.Split(physical, "?")[0]
	databases = append(databases, physical)
	if _, err := client.Database(physical).Collection("accounts").UpdateOne(ctx, bson.M{"_id": "customer"}, bson.M{"$set": bson.M{"discount": 10}}); err != nil {
		t.Fatal(err)
	}
	call("sandbox", "list", "-p", project)
	call("branches", "list", "-p", project)
	plan := call("merge", "preview", "-p", project, "-b", "candidate")["plan"].(map[string]any)
	if len(plan["changes"].([]any)) != 1 {
		t.Fatal("merge preview did not synchronize native write")
	}
	call("merge", "apply", plan["id"].(string))
	var account bson.M
	if err := client.Database(dbName).Collection("accounts").FindOne(ctx, bson.M{"_id": "customer"}).Decode(&account); err != nil {
		t.Fatal(err)
	}
	if account["discount"] != int32(10) {
		t.Fatalf("unexpected merged state: %v", account)
	}
	call("merge", "list", "-p", project)
	call("undo", "-p", project, "-b", "candidate", "--from-lsn", fmtLSN(pin["lsn"].(float64)+1), "--dry-run")
	call("sandbox", "discard", "-p", project, "-b", "candidate")
	call("pin", "delete", "-p", project, "--name", "baseline")
	call("release", "-p", project)
}

func fmtLSN(value float64) string { data, _ := json.Marshal(value); return string(data) }
