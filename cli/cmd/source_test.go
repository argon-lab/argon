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

func TestCLISourceCommandsAreDocumented(t *testing.T) {
	for _, args := range [][]string{
		{"source", "--help"}, {"source", "follow", "--help"}, {"source", "status", "--help"}, {"source", "detach", "--help"},
	} {
		if _, stderr, err := invokeCLI(t, "mongodb://127.0.0.1:1", args...); err != nil {
			t.Fatalf("%v: %v %s", args, err, stderr)
		}
	}
	stdout, _, err := invokeCLI(t, "mongodb://127.0.0.1:1", "merge", "apply", "--help")
	if err != nil || !strings.Contains(stdout, "--write-back") {
		t.Fatalf("merge apply must document --write-back: %v %s", err, stdout)
	}
	// Refused before anything is connected to.
	for _, args := range [][]string{
		{"import", "database", "--uri", "mongodb://127.0.0.1:1", "--database", "d", "--project", "p", "--online", "--source-quiesced", "--yes"},
		{"import", "database", "--uri", "mongodb://127.0.0.1:1", "--database", "d", "--project", "p", "--source-quiesced", "--enable-source-images", "--yes"},
		{"merge", "apply", primitive.NewObjectID().Hex(), "--uri", "mongodb://127.0.0.1:1"},
		{"source", "detach", "-p", "p"},
	} {
		if _, _, err := invokeCLI(t, "mongodb://127.0.0.1:1", args...); err == nil {
			t.Fatalf("accepted: %v", args)
		}
	}
}

// The whole path through the binary: import a database online, follow it,
// let an agent change a sandbox, be refused an ordinary merge, write the
// reviewed plan back, see it arrive in main through following, detach.
func TestCLIOnlineImportFollowAndWriteBack(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB integration workflow")
	}
	id := primitive.NewObjectID().Hex()
	t.Setenv("ARGON_METADATA_DB", "argon_cli_follow_test_"+id)
	// The source connection string travels in the environment, carrying a
	// marker that must never be printed or stored.
	const marker = "sourcesecretmarker"
	t.Setenv("ARGON_SOURCE_URI", uri+"&appName="+marker)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	source := client.Database("cli_follow_src_" + id)
	project := "cli-follow-" + id
	var physical string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = source.Drop(cleanup) // the test's own database
		if physical != "" {
			_ = client.Database(physical).Drop(cleanup)
		}
		_ = client.Database(metadataDatabase()).Drop(cleanup)
	}()
	if _, err := source.Collection("catalog").InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: "a"}, {Key: "name", Value: "A"}, {Key: "stock", Value: int32(1)}},
		bson.D{{Key: "_id", Value: "b"}, {Key: "name", Value: "B"}, {Key: "stock", Value: int32(2)}},
	}); err != nil {
		t.Fatal(err)
	}

	var transcript bytes.Buffer
	run := func(args ...string) (string, string, error) {
		t.Helper()
		stdout, stderr, err := invokeCLI(t, uri, args...)
		transcript.WriteString(stdout + stderr)
		return stdout, stderr, err
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

	imported := call("import", "database", "--database", source.Name(), "--project", project, "--online", "--yes")
	if imported["online"] != true || imported["consistent_lsn"].(float64) <= 0 || imported["consistent_cluster_time"].(map[string]any)["t"].(float64) <= 0 {
		t.Fatalf("online import did not report its consistent point: %v", imported)
	}
	status := call("source", "status", "-p", project)
	if status["mode"] != "exact" || status["database"] != source.Name() || len(status["inexact_ranges"].([]any)) != 1 {
		t.Fatalf("unexpected status after import: %v", status)
	}

	// Follow in a process of its own, as an operator would.
	follow := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIHelperProcess$", "--", "source", "follow", "-p", project)
	follow.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1", "MONGODB_URI="+uri)
	var followOutput bytes.Buffer
	follow.Stdout, follow.Stderr = &followOutput, &followOutput
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = follow.Process.Kill()
			_ = follow.Wait()
		}
	}()
	waitFor := func(what string, done func(map[string]any) bool) map[string]any {
		t.Helper()
		for ctx.Err() == nil {
			current := call("source", "status", "-p", project)
			if done(current) {
				return current
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; follow output: %s", what, followOutput.String())
		return nil
	}
	waitFor("the follower to run", func(s map[string]any) bool { return s["state"] == "running" })

	if _, err := source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "b"}, bson.M{"$set": bson.M{"stock": int32(20)}}); err != nil {
		t.Fatal(err)
	}
	head := status["head_lsn"].(float64)
	waitFor("the production write to reach main", func(s map[string]any) bool { return s["head_lsn"].(float64) > head })

	sandbox := call("sandbox", "create", "-p", project, "--name", "agent-1")
	physical = sandbox["physical_db"].(string)
	if _, err := client.Database(physical).Collection("catalog").UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$set": bson.M{"stock": int32(100)}}); err != nil {
		t.Fatal(err)
	}
	plan := call("merge", "preview", "-p", project, "-b", "agent-1")["plan"].(map[string]any)
	planID := plan["id"].(string)
	if len(plan["changes"].([]any)) != 1 {
		t.Fatalf("unexpected plan: %v", plan)
	}

	// Without the flag nothing is written anywhere.
	_, stderr, err := run("merge", "apply", planID)
	if err == nil || !strings.Contains(stderr, "--write-back") {
		t.Fatalf("an ordinary apply to a followed branch must be refused and name --write-back: %v %s", err, stderr)
	}
	var doc bson.M
	if err := source.Collection("catalog").FindOne(ctx, bson.M{"_id": "a"}).Decode(&doc); err != nil || doc["stock"] != int32(1) {
		t.Fatalf("the source changed without --write-back: %v %v", doc, err)
	}

	written := call("merge", "apply", planID, "--write-back")
	if written["written_to_source"] != true || written["applied"].(float64) != 1 {
		t.Fatalf("unexpected write-back result: %v", written)
	}
	if err := source.Collection("catalog").FindOne(ctx, bson.M{"_id": "a"}).Decode(&doc); err != nil || doc["stock"] != int32(100) {
		t.Fatalf("the plan did not reach the source: %v %v", doc, err)
	}
	head = call("source", "status", "-p", project)["head_lsn"].(float64)
	_ = head
	for ctx.Err() == nil {
		plans := call("merge", "list", "-p", project)["plans"].([]any)
		record, _ := plans[0].(map[string]any)["write_back"].(map[string]any)
		if record != nil && record["settled_at"] != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("following never confirmed the write-back; follow output: %s", followOutput.String())
	}
	if diff := call("diff", "-p", project, "-b", "agent-1")["plan"].(map[string]any); diff["changes"] != nil && len(diff["changes"].([]any)) != 0 {
		t.Fatalf("after the write-back was followed nothing should be left to merge: %v", diff)
	}

	// Ctrl-C saves the position and exits cleanly.
	if err := follow.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if err := follow.Wait(); err != nil {
		t.Fatalf("source follow did not exit cleanly on interrupt: %v %s", err, followOutput.String())
	}
	if !strings.Contains(followOutput.String(), "source position saved") {
		t.Fatalf("unexpected follow output: %s", followOutput.String())
	}
	transcript.Write(followOutput.Bytes())

	if _, _, err := run("source", "detach", "-p", project); err == nil {
		t.Fatal("detach must require --yes")
	}
	call("source", "detach", "-p", project, "--yes")
	if _, stderr, err := run("source", "status", "-p", project); err == nil || !strings.Contains(stderr, "does not follow") {
		t.Fatalf("a detached project still reports a source: %v %s", err, stderr)
	}

	// The connection string appeared in no output and in nothing stored.
	if strings.Contains(transcript.String(), marker) {
		t.Fatalf("the source connection string was printed:\n%s", transcript.String())
	}
	names, err := client.Database(metadataDatabase()).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		cursor, err := client.Database(metadataDatabase()).Collection(name).Find(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		for cursor.Next(ctx) {
			if strings.Contains(cursor.Current.String(), marker) {
				t.Fatalf("the source connection string was stored in %s", name)
			}
		}
	}
}
