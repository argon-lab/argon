package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
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

// A watched sandbox is capturing before its connection string exists, labels
// the run, captures a collection the run creates, and stops cleanly on
// interrupt with its writes in history.
func TestCLISandboxCreateWatch(t *testing.T) {
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
	var databases []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range databases {
			_ = client.Database(name).Drop(cleanup)
		}
		_ = client.Database(metadataDatabase()).Drop(cleanup)
	}()

	project := "cli-watch-" + primitive.NewObjectID().Hex()
	if _, stderr, err := invokeCLI(t, uri, "projects", "create", project, "--output", "json"); err != nil {
		t.Fatalf("project create failed: %v %s", err, stderr)
	}
	if _, stderr, err := invokeCLI(t, uri, "sandbox", "create", "-p", project, "--actor", "agent:run"); err == nil || !strings.Contains(stderr, "--watch") {
		t.Fatalf("--actor without --watch must be rejected: %v %s", err, stderr)
	}

	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIHelperProcess$", "--",
		"sandbox", "create", "-p", project, "--name", "watched", "--watch", "--actor", "agent:run", "--output", "json")
	command.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1", "MONGODB_URI="+uri)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	// The JSON value arrives only once capture is running.
	var sandbox map[string]any
	if err := json.NewDecoder(stdout).Decode(&sandbox); err != nil {
		t.Fatalf("no sandbox JSON: %v %s", err, stderr.String())
	}
	if sandbox["capture_managed"] != true {
		t.Fatal("a watched sandbox must report managed capture")
	}
	physical := sandbox["physical_db"].(string)
	databases = append(databases, physical)
	// The run creates this collection and updates a document at once, before
	// capture can have enabled images on it.
	orders := client.Database(physical).Collection("orders")
	if _, err := orders.InsertOne(ctx, bson.M{"_id": "o1", "total": 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := orders.UpdateOne(ctx, bson.M{"_id": "o1"}, bson.M{"$set": bson.M{"total": 6}}); err != nil {
		t.Fatal(err)
	}

	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(stdout)
	err = command.Wait()
	exited = true
	if err != nil {
		t.Fatalf("watched sandbox did not stop cleanly: %v %s", err, stderr.String())
	}
	if strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("stdout must hold one JSON value, got extra %q", rest)
	}
	count, err := client.Database(metadataDatabase()).Collection("wal_ingest_state").CountDocuments(ctx,
		bson.M{"capture_status.state": bson.M{"$exists": true, "$nin": bson.A{"stopped", "degraded"}}})
	if err != nil || count != 0 {
		t.Fatalf("command exited with active capture: count=%d error=%v", count, err)
	}

	out, errText, err := invokeCLI(t, uri, "merge", "preview", "-p", project, "-b", "watched", "--output", "json")
	if err != nil {
		t.Fatalf("preview after a watched run failed: %v %s", err, errText)
	}
	var preview map[string]any
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatal(err)
	}
	if changes := preview["plan"].(map[string]any)["changes"].([]any); len(changes) != 1 {
		t.Fatalf("expected the one new document, got %v", changes)
	}
}

// mcpProcess is "argon mcp" running as a subprocess and driven over its
// stdio, the way an MCP client runs it.
type mcpProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr string
	nextID int
	exited bool
}

func startMCPProcess(t *testing.T, uri string, args ...string) *mcpProcess {
	t.Helper()
	// The child writes its log straight to a file, so a failing step can
	// quote it while the process is still running.
	logFile, err := os.CreateTemp(t.TempDir(), "mcp-stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIHelperProcess$", "--", "mcp"}, args...)...)
	command.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1", "MONGODB_URI="+uri)
	command.Stderr = logFile
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	p := &mcpProcess{t: t, cmd: command, stdin: stdin, lines: make(chan string), stderr: logFile.Name()}
	go func() {
		defer close(p.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		if !p.exited {
			_ = command.Process.Kill()
			for range p.lines {
			}
			_ = command.Wait()
		}
	})
	return p
}

func (p *mcpProcess) log() string {
	data, _ := os.ReadFile(p.stderr)
	return string(data)
}

func (p *mcpProcess) send(message map[string]any) {
	p.t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(payload, '\n')); err != nil {
		p.t.Fatalf("server is not reading requests: %v\n%s", err, p.log())
	}
}

// call sends a request and returns the reply to it.
func (p *mcpProcess) call(method string, params any) map[string]any {
	p.t.Helper()
	p.nextID++
	request := map[string]any{"jsonrpc": "2.0", "id": p.nextID, "method": method}
	if params != nil {
		request["params"] = params
	}
	p.send(request)
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("server exited without answering %s\n%s", method, p.log())
		}
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			p.t.Fatalf("%s: stdout must carry only protocol messages, got %q", method, line)
		}
		if reply["id"] != float64(p.nextID) {
			p.t.Fatalf("%s: reply %v does not answer request %d", method, reply, p.nextID)
		}
		return reply
	case <-time.After(60 * time.Second):
		p.t.Fatalf("no reply to %s\n%s", method, p.log())
		return nil
	}
}

// result is call for a request that must succeed at the protocol level.
func (p *mcpProcess) result(method string, params any) map[string]any {
	p.t.Helper()
	reply := p.call(method, params)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		p.t.Fatalf("%s failed: %v\n%s", method, reply, p.log())
	}
	return result
}

func (p *mcpProcess) toolNames() []string {
	p.t.Helper()
	var names []string
	for _, tool := range p.result("tools/list", nil)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

// tool calls a tool and returns its text and whether it reported an error.
func (p *mcpProcess) tool(name string, args map[string]any) (string, bool) {
	p.t.Helper()
	result := p.result("tools/call", map[string]any{"name": name, "arguments": args})
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	failed, _ := result["isError"].(bool)
	return text, failed
}

// close ends the session the way a client does, by closing the server's
// input, and returns how the process exited.
func (p *mcpProcess) close() error {
	p.t.Helper()
	_ = p.stdin.Close()
	deadline := time.After(90 * time.Second)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				p.exited = true
				return p.cmd.Wait()
			}
			p.t.Fatalf("unrequested output after the last reply: %q", line)
		case <-deadline:
			p.t.Fatalf("server did not exit when its input closed\n%s", p.log())
		}
	}
}

var mcpToolNames = []string{
	"argon_branch_list", "argon_connect", "argon_diff", "argon_merge_apply", "argon_merge_preview",
	"argon_pin_create", "argon_pin_list", "argon_pin_sandbox", "argon_sandbox_create",
	"argon_sandbox_discard", "argon_sandbox_keep", "argon_snapshot_create", "argon_undo",
}

// A client that only wants to know what the server offers, such as a directory
// that lists it, starts it with no database. The handshake and the tool list
// must not depend on MongoDB; a tool call says what is missing and leaves the
// server running.
func TestCLIMCPServesWithoutMongoDB(t *testing.T) {
	const unreachable = "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100"

	p := startMCPProcess(t, unreachable)
	initialized := p.result("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if initialized["protocolVersion"] != "2025-06-18" {
		t.Fatalf("unexpected handshake: %v", initialized)
	}
	if info := initialized["serverInfo"].(map[string]any); info["name"] != "argon" || info["version"] != rootCmd.Version {
		t.Fatalf("unexpected server info: %v", info)
	}
	p.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if result := p.result("ping", nil); len(result) != 0 {
		t.Fatalf("unexpected ping result: %v", result)
	}
	if names := p.toolNames(); !reflect.DeepEqual(names, mcpToolNames) {
		t.Fatalf("tools/list without a database: %v", names)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		text, failed := p.tool("argon_branch_list", map[string]any{"project": "demo"})
		if !failed {
			t.Fatalf("attempt %d: a tool cannot succeed without MongoDB: %q", attempt, text)
		}
		for _, want := range []string{"MongoDB is not reachable", "MONGODB_URI", "replica set"} {
			if !strings.Contains(text, want) {
				t.Fatalf("attempt %d: the tool error should mention %q: %q", attempt, want, text)
			}
		}
		// The failed call is an answer, not a crash.
		if result := p.result("ping", nil); len(result) != 0 {
			t.Fatalf("unexpected ping result: %v", result)
		}
	}
	if names := p.toolNames(); !reflect.DeepEqual(names, mcpToolNames) {
		t.Fatalf("tools/list after a failed call: %v", names)
	}
	if err := p.close(); err != nil {
		t.Fatalf("a server that never reached MongoDB must still exit cleanly: %v\n%s", err, p.log())
	}

	// Restrictions are applied to the list without a database as well.
	readOnly := startMCPProcess(t, unreachable, "--read-only")
	if names := readOnly.toolNames(); !reflect.DeepEqual(names, []string{"argon_branch_list", "argon_diff", "argon_pin_list"}) {
		t.Fatalf("--read-only without a database: %v", names)
	}
	if err := readOnly.close(); err != nil {
		t.Fatalf("%v\n%s", err, readOnly.log())
	}
	restricted := startMCPProcess(t, unreachable, "--disable-tools", "argon_merge_apply,argon_undo")
	names := restricted.toolNames()
	if len(names) != len(mcpToolNames)-2 || strings.Contains(strings.Join(names, " "), "argon_merge_apply") || strings.Contains(strings.Join(names, " "), "argon_undo") {
		t.Fatalf("--disable-tools without a database: %v", names)
	}
	// A withheld tool is refused as before, without a connection attempt.
	if reply := restricted.call("tools/call", map[string]any{"name": "argon_undo", "arguments": map[string]any{}}); reply["error"] == nil {
		t.Fatalf("a withheld tool must be refused: %v", reply)
	}
	if err := restricted.close(); err != nil {
		t.Fatalf("%v\n%s", err, restricted.log())
	}
	if _, stderr, err := invokeCLI(t, unreachable, "mcp", "--disable-tools", "argon_merge_aply"); err == nil || !strings.Contains(stderr, "unknown tool") {
		t.Fatalf("a misspelled tool name must still be rejected at startup: %v %s", err, stderr)
	}
}

// gatedListener stands on a loopback port in place of MongoDB. Until open is
// called every connection is dropped at once, as on a port where nothing
// serves yet; afterwards connections are relayed to the real deployment. The
// address stays the same, as it does when a database comes up.
type gatedListener struct {
	listener net.Listener
	target   string
	serving  atomic.Bool
}

func newGatedListener(t *testing.T, target string) *gatedListener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gatedListener{listener: listener, target: target}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go g.relay(conn)
		}
	}()
	return g
}

func (g *gatedListener) relay(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if !g.serving.Load() {
		return
	}
	upstream, err := net.Dial("tcp", g.target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(conn, upstream)
}

func (g *gatedListener) open() { g.serving.Store(true) }

// The short server selection only shortens the call made before the gate
// opens; it is generous for everything after it.
func (g *gatedListener) uri() string {
	return "mongodb://" + g.listener.Addr().String() + "/?directConnection=true&serverSelectionTimeoutMS=2000"
}

// The server is started before its database. When MongoDB comes up at the
// same address the next tool call connects, the sandbox it hands out is
// captured, and closing the session stops capture and exits cleanly.
func TestCLIMCPConnectsWhenMongoDBBecomesReachable(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB integration workflow")
	}
	t.Setenv("ARGON_METADATA_DB", "argon_cli_test_"+primitive.NewObjectID().Hex())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	var databases []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range databases {
			_ = client.Database(name).Drop(cleanup)
		}
		_ = client.Database(metadataDatabase()).Drop(cleanup)
	}()
	var hello struct {
		Primary string `bson:"primary"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || hello.Primary == "" {
		t.Fatalf("no replica-set primary to relay to: %q %v", hello.Primary, err)
	}

	project := "cli-mcp-" + primitive.NewObjectID().Hex()
	if _, stderr, err := invokeCLI(t, uri, "projects", "create", project, "--output", "json"); err != nil {
		t.Fatalf("project create failed: %v %s", err, stderr)
	}

	gate := newGatedListener(t, hello.Primary)
	p := startMCPProcess(t, gate.uri())
	p.result("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if names := p.toolNames(); !reflect.DeepEqual(names, mcpToolNames) {
		t.Fatalf("tools/list before MongoDB is up: %v", names)
	}
	text, failed := p.tool("argon_branch_list", map[string]any{"project": project})
	if !failed || !strings.Contains(text, "MongoDB is not reachable") {
		t.Fatalf("a call before MongoDB is up must say so: %v %q", failed, text)
	}

	gate.open()
	text, failed = p.tool("argon_branch_list", map[string]any{"project": project})
	if failed || !strings.Contains(text, "main: head LSN") {
		t.Fatalf("the call after MongoDB came up must connect: %v %q\n%s", failed, text, p.log())
	}

	// The late connection supervises capture like one made at startup.
	text, failed = p.tool("argon_sandbox_create", map[string]any{"project": project, "name": "run-1", "actor": "agent:run"})
	if failed || !strings.Contains(text, "Connection string: ") {
		t.Fatalf("sandbox create failed: %q\n%s", text, p.log())
	}
	sandboxURI := strings.Fields(strings.SplitN(text, "Connection string: ", 2)[1])[0]
	physical := strings.Split(strings.Split(strings.Split(sandboxURI, "://")[1], "/")[1], "?")[0]
	databases = append(databases, physical)
	agent, err := mongo.Connect(ctx, options.Client().ApplyURI(sandboxURI))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agent.Disconnect(context.Background()) }()
	if _, err := agent.Database(physical).Collection("orders").InsertOne(ctx, bson.M{"_id": "o1", "total": 5}); err != nil {
		t.Fatalf("the sandbox connection string must work as handed out: %v", err)
	}
	text, failed = p.tool("argon_diff", map[string]any{"project": project, "branch": "run-1"})
	if failed || !strings.Contains(text, "put orders/o1") {
		t.Fatalf("the sandbox write was not captured: %q\n%s", text, p.log())
	}

	if err := p.close(); err != nil {
		t.Fatalf("server did not exit cleanly: %v\n%s", err, p.log())
	}
	count, err := client.Database(metadataDatabase()).Collection("wal_ingest_state").CountDocuments(ctx,
		bson.M{"capture_status.state": bson.M{"$exists": true, "$nin": bson.A{"stopped", "degraded"}}})
	if err != nil || count != 0 {
		t.Fatalf("server exited with active capture: count=%d error=%v", count, err)
	}
	stopped, err := client.Database(metadataDatabase()).Collection("wal_ingest_state").CountDocuments(ctx,
		bson.M{"capture_status.state": "stopped", "capture_status.actor": "agent:run"})
	if err != nil || stopped != 1 {
		t.Fatalf("the sandbox's capture should be recorded as stopped: count=%d error=%v", stopped, err)
	}
}
