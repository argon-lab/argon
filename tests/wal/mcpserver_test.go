package wal_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/gc"
	"github.com/argon-lab/argon/internal/ingest"
	"github.com/argon-lab/argon/internal/merge"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/sandbox"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/undo"
	"github.com/argon-lab/argon/pkg/mcpserver"
	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mcpClient drives an in-process MCP server over pipes.
type mcpClient struct {
	t      *testing.T
	in     io.WriteCloser
	out    *bufio.Scanner
	nextID int
}

func startMCP(t *testing.T, db *mongo.Database) (*mcpClient, *walcli.Services) {
	t.Helper()
	f := newSnapshotFixture(t, db)
	client := db.Client()
	co := checkout.NewService(client, db, f.branches, f.mat)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	f.branches.SetDeleteHook(func(branchID string) {
		_, _, _, _ = gcService.ReclaimDeletedBranch(context.Background(), branchID)
	})
	projectService, err := projectwal.NewProjectService(db, f.wal, f.branches)
	require.NoError(t, err)

	services := &walcli.Services{
		WAL:          f.wal,
		Branches:     f.branches,
		Projects:     projectService,
		Materializer: f.mat,
		TimeTravel:   timetravel.NewService(f.wal, f.mat),
		Snapshots:    f.snapshots,
		GC:           gcService,
		Checkout:     co,
		Ingest:       ingest.NewService(client, db, f.wal, f.branches),
		Undo:         undo.NewService(f.wal, f.branches, client),
		Merge:        merge.NewService(db, f.wal, f.branches, f.mat, client),
		Sandbox:      sandbox.NewService(f.branches, co),
		MongoURI:     "mongodb://localhost:27017",
	}

	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	server := mcpserver.New(services, serverIn, serverOut)
	go func() { _ = server.Run(context.Background()) }()
	t.Cleanup(func() { _ = clientOut.Close() })

	scanner := bufio.NewScanner(clientIn)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &mcpClient{t: t, in: clientOut, out: scanner}, services
}

// call sends a request and decodes the matching response.
func (c *mcpClient) call(method string, params interface{}) map[string]interface{} {
	c.t.Helper()
	c.nextID++
	req := map[string]interface{}{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	payload, err := json.Marshal(req)
	require.NoError(c.t, err)
	_, err = c.in.Write(append(payload, '\n'))
	require.NoError(c.t, err)

	require.True(c.t, c.out.Scan(), "server closed the stream")
	var resp map[string]interface{}
	require.NoError(c.t, json.Unmarshal(c.out.Bytes(), &resp))
	require.EqualValues(c.t, c.nextID, resp["id"], "response id matches request")
	require.Nil(c.t, resp["error"], "unexpected protocol error: %v", resp["error"])
	return resp["result"].(map[string]interface{})
}

// toolText calls a tool and returns its text payload.
func (c *mcpClient) toolText(name string, args map[string]interface{}) (string, bool) {
	c.t.Helper()
	result := c.call("tools/call", map[string]interface{}{"name": name, "arguments": args})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	isError, _ := result["isError"].(bool)
	return text, isError
}

func TestMCP_ProtocolAndAgentLoop(t *testing.T) {
	db := setupTestDB(t)
	c, services := startMCP(t, db)
	ctx := context.Background()

	// Handshake.
	init := c.call("initialize", map[string]interface{}{"protocolVersion": "2024-11-05"})
	assert.Equal(t, "argon", init["serverInfo"].(map[string]interface{})["name"])

	// Tool inventory.
	list := c.call("tools/list", nil)
	tools := list["tools"].([]interface{})
	names := make(map[string]bool)
	for _, tl := range tools {
		names[tl.(map[string]interface{})["name"].(string)] = true
	}
	for _, want := range []string{"argon_sandbox_create", "argon_connect", "argon_diff", "argon_merge_preview", "argon_merge_apply", "argon_undo", "argon_sandbox_discard"} {
		assert.True(t, names[want], "tool %s advertised", want)
	}

	// Seed a project through the services.
	project, err := services.Projects.CreateProject("agent-loop")
	require.NoError(t, err)
	writer, err := services.WriterFor("agent-loop", "main")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "prod", "v": int32(1)})
	require.NoError(t, err)
	_ = project

	// 1. The agent forks a sandbox and gets a connection string.
	text, isErr := c.toolText("argon_sandbox_create", map[string]interface{}{
		"project": "agent-loop", "name": "agent-sbx", "ttl_minutes": 30,
	})
	require.False(t, isErr, text)
	assert.Contains(t, text, "Connection string:")
	uriMatch := regexp.MustCompile(`mongodb://\S+`).FindString(text)
	require.NotEmpty(t, uriMatch)
	physName := uriMatch[strings.LastIndex(uriMatch, "/")+1:]
	t.Cleanup(func() { _ = db.Client().Database(physName).Drop(context.Background()) })

	// 2. The agent writes through a plain MongoDB driver.
	agentClient, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)
	defer func() { _ = agentClient.Disconnect(context.Background()) }()
	agentDB := agentClient.Database(physName)
	_, err = agentDB.Collection("docs").InsertOne(ctx, bson.M{"_id": "agent-made", "v": int32(42)})
	require.NoError(t, err)

	// The server-supervised ingester captures it.
	sbx, err := services.Branches.GetBranch(project.ID, "agent-sbx")
	require.NoError(t, err)
	deadline := time.Now().Add(20 * time.Second)
	for {
		branch, err := services.Branches.GetBranchByID(sbx.ID)
		require.NoError(t, err)
		entries, err := services.WAL.GetBranchEntries(sbx.ID, "docs", 0, branch.HeadLSN)
		require.NoError(t, err)
		if len(entries) >= 1 {
			break
		}
		require.True(t, time.Now().Before(deadline), "ingester never captured the agent write")
		time.Sleep(100 * time.Millisecond)
	}

	// 3. The agent inspects the diff and merges back.
	text, isErr = c.toolText("argon_diff", map[string]interface{}{"project": "agent-loop", "branch": "agent-sbx"})
	require.False(t, isErr, text)
	assert.Contains(t, text, "put docs/agent-made")

	text, isErr = c.toolText("argon_merge_preview", map[string]interface{}{"project": "agent-loop", "branch": "agent-sbx"})
	require.False(t, isErr, text)
	planID := regexp.MustCompile(`[0-9a-f]{24}`).FindString(text)
	require.NotEmpty(t, planID, "preview returns a plan id: %s", text)

	text, isErr = c.toolText("argon_merge_apply", map[string]interface{}{"plan_id": planID})
	require.False(t, isErr, text)
	assert.Contains(t, text, "1 change(s) applied")

	main, err := services.Branches.GetBranch(project.ID, "main")
	require.NoError(t, err)
	state, err := services.Materializer.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.Contains(t, state, "agent-made", "the agent's work landed on main")

	// 4. The undo button, from the agent's point of view (dry run).
	text, isErr = c.toolText("argon_undo", map[string]interface{}{
		"project": "agent-loop", "branch": "main",
		"from_lsn": float64(main.HeadLSN), "dry_run": true,
	})
	require.False(t, isErr, text)
	assert.Contains(t, text, "Dry run")

	// 5. Discard the sandbox; storage reclaims.
	text, isErr = c.toolText("argon_sandbox_discard", map[string]interface{}{"project": "agent-loop", "branch": "agent-sbx"})
	require.False(t, isErr, text)
	_, err = services.Branches.GetBranchByID(sbx.ID)
	assert.Error(t, err, "sandbox gone")

	// Tool-level failures surface as isError results, not protocol errors.
	text, isErr = c.toolText("argon_connect", map[string]interface{}{"project": "no-such", "branch": "main"})
	assert.True(t, isErr)
	assert.Contains(t, text, "not found")

	// Unknown methods are proper JSON-RPC errors.
	c.nextID++
	payload, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": c.nextID, "method": "bogus/method"})
	_, err = c.in.Write(append(payload, '\n'))
	require.NoError(t, err)
	require.True(t, c.out.Scan())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(c.out.Bytes(), &resp))
	require.NotNil(t, resp["error"])
	assert.EqualValues(t, -32601, resp["error"].(map[string]interface{})["code"])

}
