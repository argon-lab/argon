package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
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

func TestCLIImportRequiresStoppedSourceBeforeConnecting(t *testing.T) {
	_, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1", "import", "database",
		"--uri", "mongodb://127.0.0.1:1", "--database", "source", "--project", "target", "--yes")
	if err == nil || !strings.Contains(stderr, "--source-quiesced") {
		t.Fatalf("import must reject a missing source acknowledgment before connecting: %v %s", err, stderr)
	}
	_, stderr, err = invokeCLI(t, "mongodb://127.0.0.1:1", "import", "cleanup", "--project", "target")
	if err == nil || !strings.Contains(stderr, "stop the original importer") {
		t.Fatalf("cleanup must require stopped-importer confirmation: %v %s", err, stderr)
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
	defer func() { _ = client.Disconnect(context.Background()) }()
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
	restore := func(verb string, args ...string) (string, string, error) {
		t.Helper()
		out, stderr, err := invokeCLI(t, uri, append([]string{"restore", verb, "-p", project}, args...)...)
		assertCaptureStopped()
		return out, stderr, err
	}
	mustRestore := func(want, verb string, args ...string) {
		t.Helper()
		out, stderr, err := restore(verb, args...)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("restore %s %v: want %q, got %q: %v %s", verb, args, want, out, err, stderr)
		}
	}
	if _, stderr, err := restore("reset", "--lsn", "0"); err == nil || !strings.Contains(stderr, "before resetting a live branch") {
		t.Fatalf("zero is a valid target but cannot reset a live branch: %v %s", err, stderr)
	}
	call("release", "-p", project)
	mustRestore("Target:  LSN 0\n", "preview", "--lsn", "0")
	positiveLSN := fmtLSN(pin["lsn"].(float64))
	mustRestore("Target:  LSN "+positiveLSN+"\n", "preview", "--lsn", positiveLSN)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	mustRestore("Target:  LSN ", "preview", "--time", at)
	before := call("branches", "list", "-p", project)
	if _, stderr, err := restore("reset", "--lsn", "0", "--time", at, "--backup", "invalid-mixed-backup"); err == nil || !strings.Contains(stderr, "exactly one of --lsn or --time is required") {
		t.Fatalf("restore must reject mixed zero LSN and time: %v %s", err, stderr)
	}
	if after := call("branches", "list", "-p", project); !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid restore changed branch metadata: before=%v after=%v", before, after)
	}
	mustRestore("Created branch \"initial\" at LSN 0", "branch", "--lsn", "0", "--as", "initial")
	mustRestore("Reset main to LSN 0", "reset", "--lsn", "0")
	for _, branch := range []string{"main", "initial"} {
		state := call("time-travel", "query", "-p", project, "-b", branch, "--lsn", "0", "-c", "accounts")
		if len(state["documents"].(map[string]any)) != 0 {
			t.Fatalf("branch %s should have no documents at its initial state", branch)
		}
	}
}

func fmtLSN(value float64) string { data, _ := json.Marshal(value); return string(data) }

// The proxy forwards to MongoDB without authenticating clients, so it must
// not listen beyond loopback unless that was asked for explicitly.
func TestProxyListensOnLoopbackUnlessRemoteAccessIsExplicit(t *testing.T) {
	if got := proxyCmd.Flags().Lookup("listen").DefValue; !isLoopbackListen(got) {
		t.Fatalf("default --listen %q is not loopback", got)
	}
	for _, addr := range []string{"127.0.0.1:27018", "localhost:27018", "[::1]:27018", "127.8.9.10:1"} {
		if !isLoopbackListen(addr) {
			t.Errorf("%s should count as loopback", addr)
		}
		if err := validateProxyListen(addr, false); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":27018", "0.0.0.0:27018", "[::]:27018", "192.168.1.20:27018", "db.internal:27018"} {
		if isLoopbackListen(addr) {
			t.Errorf("%s must not count as loopback", addr)
		}
		err := validateProxyListen(addr, false)
		if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
			t.Errorf("%s: expected a refusal naming --allow-remote, got %v", addr, err)
		}
		if err := validateProxyListen(addr, true); err != nil {
			t.Errorf("%s with --allow-remote: %v", addr, err)
		}
	}
	if err := validateProxyListen("no-port", true); err == nil {
		t.Error("an address without a port must be rejected")
	}

	// The refusal happens before any connection is attempted.
	_, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100", "proxy", "--listen", ":27018")
	if err == nil || !strings.Contains(stderr, "--allow-remote") {
		t.Fatalf("expected the CLI to refuse a public listener: %v %s", err, stderr)
	}
}

// The documented incident flow, end to end through the CLI: reset with
// --backup, then reset again because the first target was not far enough
// back. The backup and an earlier pin must keep reading everything; a reset
// into the discarded stretch is refused without side effects; forking that
// stretch recovers it.
func TestCLIRepeatedResetsKeepBackupAndPin(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB integration workflow")
	}
	t.Setenv("ARGON_METADATA_DB", "argon_cli_reset_test_"+primitive.NewObjectID().Hex())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	run := func(args ...string) (string, string, error) {
		t.Helper()
		return invokeCLI(t, uri, args...)
	}
	call := func(args ...string) map[string]any {
		t.Helper()
		stdout, stderr, err := run(append(args, "--output", "json")...)
		if err != nil {
			t.Fatalf("%v failed: %v %s %s", args, err, stdout, stderr)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("%v output is not one JSON value: %q (%v)", args, stdout, err)
		}
		return result
	}
	project := "cli-reset-" + primitive.NewObjectID().Hex()
	call("projects", "create", project)
	checkout := call("checkout", "-p", project)
	physical := checkout["physical_db"].(string)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Database(physical).Drop(cleanup)
		_ = client.Database(metadataDatabase()).Drop(cleanup)
	}()
	call("collections", "prepare", "orders", "-p", project)

	// Three batches of orders; a pin after each records where it ended.
	orders := client.Database(physical).Collection("orders")
	batch := func(prefix, pin string) string {
		t.Helper()
		for i := 0; i < 3; i++ {
			if _, err := orders.InsertOne(ctx, bson.M{"_id": fmt.Sprintf("%s%d", prefix, i)}); err != nil {
				t.Fatal(err)
			}
		}
		return fmtLSN(call("pin", "create", "-p", project, "--name", pin)["pin"].(map[string]any)["lsn"].(float64))
	}
	afterK := batch("k", "after-k")
	afterM := batch("m", "after-m")
	afterX := batch("x", "everything")
	call("release", "-p", project)

	count := func(branch, lsn string) int {
		t.Helper()
		state := call("time-travel", "query", "-p", project, "-b", branch, "--lsn", lsn, "-c", "orders")
		return len(state["documents"].(map[string]any))
	}
	reset := func(args ...string) (string, string, error) {
		t.Helper()
		return run(append([]string{"restore", "reset", "-p", project}, args...)...)
	}

	if out, stderr, err := reset("--lsn", afterM, "--backup", "pre-incident"); err != nil || !strings.Contains(out, "Reset main to LSN "+afterM) {
		t.Fatalf("first reset: %v %s %s", err, out, stderr)
	}
	if got := count("pre-incident", afterX); got != 9 {
		t.Fatalf("backup after the first reset: %d documents, want 9", got)
	}
	if out, stderr, err := reset("--lsn", afterK); err != nil || !strings.Contains(out, "Reset main to LSN "+afterK) {
		t.Fatalf("second reset: %v %s %s", err, out, stderr)
	}
	if got := count("pre-incident", afterX); got != 9 {
		t.Fatalf("backup after the second reset: %d documents, want 9", got)
	}
	call("pin", "branch", "-p", project, "--name", "everything", "--as", "from-pin")
	if got := count("from-pin", afterX); got != 9 {
		t.Fatalf("branch from the earlier pin: %d documents, want 9", got)
	}
	if got := count("main", afterK); got != 3 {
		t.Fatalf("main after both resets: %d documents, want 3", got)
	}

	// afterM is now inside discarded history: not a reset target, and the
	// refusal must not leave a backup branch behind.
	before := call("branches", "list", "-p", project)
	if _, stderr, err := reset("--lsn", afterM, "--backup", "should-not-exist"); err == nil || !strings.Contains(stderr, "earlier reset discarded") {
		t.Fatalf("reset into discarded history must be refused: %v %s", err, stderr)
	}
	if _, stderr, err := run("restore", "preview", "-p", project, "--lsn", afterM); err == nil || !strings.Contains(stderr, "earlier reset discarded") {
		t.Fatalf("preview must fail the way the reset would: %v %s", err, stderr)
	}
	if after := call("branches", "list", "-p", project); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused reset changed branch metadata: before=%v after=%v", before, after)
	}

	// The discarded stretch is still readable and forkable.
	if got := count("main", afterM); got != 6 {
		t.Fatalf("historical read of discarded LSN %s: %d documents, want 6", afterM, got)
	}
	if out, stderr, err := run("restore", "branch", "-p", project, "--lsn", afterM, "--as", "recovered"); err != nil || !strings.Contains(out, `Created branch "recovered" at LSN `+afterM) {
		t.Fatalf("recovering discarded history: %v %s %s", err, out, stderr)
	}
	if got := count("recovered", afterM); got != 6 {
		t.Fatalf("recovered branch: %d documents, want 6", got)
	}
	// Reading beyond anything the branch ever held is an error, with or
	// without a collection filter.
	beyond := fmtLSN(1 << 40)
	for _, extra := range [][]string{{"-c", "orders"}, nil} {
		args := append([]string{"time-travel", "query", "-p", project, "--lsn", beyond}, extra...)
		if _, stderr, err := run(args...); err == nil || !strings.Contains(stderr, "beyond branch HEAD") {
			t.Fatalf("%v must be rejected: %v %s", args, err, stderr)
		}
	}
}
