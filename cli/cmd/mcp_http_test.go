package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCLIMCPHTTPRefusesAnOpenListenerWithoutAToken(t *testing.T) {
	for _, tc := range []struct{ addr, token, want string }{
		{"0.0.0.0:0", "", "needs ARGON_MCP_TOKEN"},
		{":0", "", "needs ARGON_MCP_TOKEN"},
		{"127.0.0.1:0", "short", "at least 32 characters"},
	} {
		if err := checkHTTPListen(tc.addr, tc.token); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s token %q: want %q, got %v", tc.addr, tc.token, tc.want, err)
		}
	}
	for _, tc := range []struct{ addr, token string }{
		{"127.0.0.1:0", ""},
		{"0.0.0.0:0", strings.Repeat("x", 32)},
	} {
		if err := checkHTTPListen(tc.addr, tc.token); err != nil {
			t.Fatalf("%s: %v", tc.addr, err)
		}
	}
}

type httpMCP struct {
	t     *testing.T
	url   string
	token string
	id    int
}

func (c *httpMCP) call(method string, params any) (int, map[string]any) {
	c.t.Helper()
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	return resp.StatusCode, reply
}

func (c *httpMCP) tool(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	status, reply := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if status != http.StatusOK || reply["result"] == nil {
		c.t.Fatalf("%s: status %d reply %v", name, status, reply)
	}
	result := reply["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	failed, _ := result["isError"].(bool)
	return text, failed
}

// The HTTP transport runs the same agent loop as stdio, behind a bearer
// token, and stops capture cleanly on SIGTERM.
func TestCLIMCPOverHTTP(t *testing.T) {
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
	project := "cli-mcp-http-" + primitive.NewObjectID().Hex()
	if _, stderr, err := invokeCLI(t, uri, "projects", "create", project, "--output", "json"); err != nil {
		t.Fatalf("project create failed: %v %s", err, stderr)
	}

	token := strings.Repeat("t0k3n", 8)
	command := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$", "--", "mcp", "--http", "127.0.0.1:0")
	command.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1", "MONGODB_URI="+uri, "ARGON_MCP_TOKEN="+token)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
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
	var logs strings.Builder
	address := make(chan string, 1)
	go func() {
		pattern := regexp.MustCompile(`at (http://\S+/mcp)`)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logs.WriteString(scanner.Text() + "\n")
			if m := pattern.FindStringSubmatch(scanner.Text()); m != nil {
				address <- m[1]
			}
		}
	}()
	var endpoint string
	select {
	case endpoint = <-address:
	case <-time.After(30 * time.Second):
		t.Fatalf("server did not report its address:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("the token must not be logged")
	}

	anonymous := &httpMCP{t: t, url: endpoint}
	if status, _ := anonymous.call("tools/list", nil); status != http.StatusUnauthorized {
		t.Fatalf("a request without the token must be refused, got %d", status)
	}

	c := &httpMCP{t: t, url: endpoint, token: token}
	if status, reply := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"}); status != http.StatusOK || reply["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", status, reply)
	}
	text, failed := c.tool("argon_sandbox_create", map[string]any{"project": project, "name": "run-1", "actor": "agent:http"})
	if failed || !strings.Contains(text, "Connection string: ") {
		t.Fatalf("sandbox create failed: %q\n%s", text, logs.String())
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
		t.Fatal(err)
	}
	// Capture is supervised by the server process, not by the request that
	// created the sandbox, so the write lands after that request ended.
	deadline := time.Now().Add(20 * time.Second)
	for {
		text, failed = c.tool("argon_diff", map[string]any{"project": project, "branch": "run-1"})
		if !failed && strings.Contains(text, "put orders/o1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox write was not captured: %q\n%s", text, logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		exited = true
		if err != nil {
			t.Fatalf("server did not exit cleanly on SIGTERM: %v\n%s", err, logs.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("server did not exit on SIGTERM:\n%s", logs.String())
	}
	stopped, err := client.Database(metadataDatabase()).Collection("wal_ingest_state").CountDocuments(ctx,
		bson.M{"capture_status.state": "stopped", "capture_status.actor": "agent:http"})
	if err != nil || stopped != 1 {
		t.Fatalf("the sandbox's capture should be recorded as stopped: count=%d error=%v", stopped, err)
	}
}
