package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/version"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchange sends one request through a server that has no engine behind it
// and decodes the reply. Listing tools, the handshake and refusals never
// reach the engine.
func exchange(t *testing.T, s *Server, method string, params interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	out := s.out.(*bytes.Buffer)
	out.Reset()
	s.dispatch(context.Background(), &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: raw})
	var reply map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &reply), out.String())
	return reply
}

func newBareServer(opts ...Option) *Server {
	return New(nil, bytes.NewReader(nil), &bytes.Buffer{}, opts...)
}

func listedTools(t *testing.T, s *Server) map[string]map[string]interface{} {
	t.Helper()
	result := exchange(t, s, "tools/list", nil)["result"].(map[string]interface{})
	tools := make(map[string]map[string]interface{})
	for _, raw := range result["tools"].([]interface{}) {
		tool := raw.(map[string]interface{})
		tools[tool["name"].(string)] = tool
	}
	return tools
}

func TestInitializeReportsTheRealVersionAndNegotiatesTheRevision(t *testing.T) {
	s := newBareServer()
	for requested, want := range map[string]string{
		"2024-11-05": "2024-11-05",
		"2025-03-26": "2025-03-26",
		"2025-06-18": "2025-06-18",
		"2099-01-01": "2025-06-18", // unknown: answer with the newest we speak
		"":           "2025-06-18",
	} {
		result := exchange(t, s, "initialize", map[string]interface{}{"protocolVersion": requested})["result"].(map[string]interface{})
		assert.Equal(t, want, result["protocolVersion"], "requested %q", requested)
		info := result["serverInfo"].(map[string]interface{})
		assert.Equal(t, "argon", info["name"])
		assert.Equal(t, version.String(), info["version"])
	}
}

func TestEveryToolDeclaresItsBehaviour(t *testing.T) {
	tools := listedTools(t, newBareServer())
	require.Len(t, tools, len(toolHandlers))
	for name := range toolHandlers {
		tool, ok := tools[name]
		require.True(t, ok, "%s is listed", name)
		_, described := toolTraits[name]
		require.True(t, described, "%s has traits", name)
		assert.NotEmpty(t, tool["title"], name)
		annotations := tool["annotations"].(map[string]interface{})
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			_, present := annotations[hint].(bool)
			assert.True(t, present, "%s declares %s", name, hint)
		}
		assert.False(t, annotations["readOnlyHint"].(bool) && annotations["destructiveHint"].(bool), "%s cannot be both", name)
	}
	// The operations that can lose or overwrite data are the ones a client
	// should stop and ask about.
	var destructive []string
	for name, tool := range tools {
		if tool["annotations"].(map[string]interface{})["destructiveHint"].(bool) {
			destructive = append(destructive, name)
		}
	}
	sort.Strings(destructive)
	assert.Equal(t, []string{"argon_merge_apply", "argon_sandbox_discard", "argon_undo"}, destructive)
}

func TestWithheldToolsAreNeitherListedNorCallable(t *testing.T) {
	call := func(s *Server, tool string) map[string]interface{} {
		return exchange(t, s, "tools/call", map[string]interface{}{"name": tool, "arguments": map[string]interface{}{"plan_id": "x", "project": "p"}})
	}

	s := newBareServer(DisableTools("argon_merge_apply"))
	tools := listedTools(t, s)
	assert.NotContains(t, tools, "argon_merge_apply")
	assert.Contains(t, tools, "argon_merge_preview")
	assert.Len(t, tools, len(toolHandlers)-1)
	reply := call(s, "argon_merge_apply")
	require.NotNil(t, reply["error"], "a withheld tool is refused even when called blind")
	assert.Contains(t, reply["error"].(map[string]interface{})["message"], "disabled")

	readOnly := newBareServer(ReadOnly())
	var names []string
	for name := range listedTools(t, readOnly) {
		names = append(names, name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"argon_branch_list", "argon_diff", "argon_pin_list"}, names)
	for _, tool := range []string{"argon_sandbox_create", "argon_undo", "argon_merge_apply", "argon_connect"} {
		assert.NotNil(t, call(readOnly, tool)["error"], tool)
	}

	assert.Equal(t, []string{"argon_merge_aply"}, UnknownTools([]string{"argon_merge_apply", "argon_merge_aply", ""}))
}

// session drives a server through Run over pipes, the way a client does:
// one loop reads the requests and answers them in order.
type session struct {
	t      *testing.T
	in     io.WriteCloser
	out    *bufio.Scanner
	done   chan struct{}
	runErr error
	nextID int
}

func serve(t *testing.T, build func(in io.Reader, out io.Writer) *Server) *session {
	t.Helper()
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	c := &session{t: t, in: clientOut, out: bufio.NewScanner(clientIn), done: make(chan struct{})}
	c.out.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	server := build(serverIn, serverOut)
	go func() {
		c.runErr = server.Run(context.Background())
		// A server that stopped must fail the next exchange, not block it.
		_ = serverOut.Close()
		_ = serverIn.Close()
		close(c.done)
	}()
	// A test that fails halfway still waits for the server's own shutdown
	// before anything it used is taken away.
	t.Cleanup(func() { _ = c.end() })
	return c
}

func (c *session) send(message map[string]interface{}) {
	c.t.Helper()
	payload, err := json.Marshal(message)
	require.NoError(c.t, err)
	_, err = c.in.Write(append(payload, '\n'))
	require.NoError(c.t, err, "the server stopped reading requests")
}

// call sends a request and returns the reply to it.
func (c *session) call(method string, params interface{}) map[string]interface{} {
	c.t.Helper()
	c.nextID++
	request := map[string]interface{}{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		request["params"] = params
	}
	c.send(request)
	require.True(c.t, c.out.Scan(), "the server stopped without answering %s", method)
	var reply map[string]interface{}
	require.NoError(c.t, json.Unmarshal(c.out.Bytes(), &reply), c.out.Text())
	require.EqualValues(c.t, c.nextID, reply["id"], "the reply answers the request")
	return reply
}

// result is call for a request that must succeed at the protocol level.
func (c *session) result(method string, params interface{}) map[string]interface{} {
	c.t.Helper()
	reply := c.call(method, params)
	require.Nil(c.t, reply["error"], "%s", method)
	return reply["result"].(map[string]interface{})
}

func (c *session) tools() map[string]map[string]interface{} {
	c.t.Helper()
	tools := make(map[string]map[string]interface{})
	for _, raw := range c.result("tools/list", nil)["tools"].([]interface{}) {
		tool := raw.(map[string]interface{})
		tools[tool["name"].(string)] = tool
	}
	return tools
}

// tool calls a tool and returns its text and whether it reported an error.
func (c *session) tool(name string, args map[string]interface{}) (string, bool) {
	c.t.Helper()
	result := c.result("tools/call", map[string]interface{}{"name": name, "arguments": args})
	text := result["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	failed, _ := result["isError"].(bool)
	return text, failed
}

// end closes the server's input, as a client that is done does, and returns
// what Run returned.
func (c *session) end() error {
	_ = c.in.Close()
	select {
	case <-c.done:
		return c.runErr
	case <-time.After(90 * time.Second):
		return errors.New("Run did not return when its input closed")
	}
}

func goroutines() string {
	buf := make([]byte, 1<<20)
	for {
		if n := runtime.Stack(buf, true); n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// sweepers counts the goroutines reaping expired sandboxes: the background
// work a server runs for as long as it has an engine.
func sweepers() int {
	return strings.Count(goroutines(), "walcli.(*Services).RunSandboxSweeper(")
}

const (
	nothingListening = "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100"
	replicaSet       = "mongodb://localhost:27017"
)

var unreachableWording = []string{"MongoDB is not reachable", "MONGODB_URI", "replica set"}

// A client that only wants to know what the server offers, such as a
// directory that lists it, starts it with no database. The handshake and the
// tool list must not open a connection.
func TestIntrospectionNeedsNoDatabase(t *testing.T) {
	var attempts atomic.Int32
	connect := func(context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		return nil, errors.New("introspection must not connect")
	}
	lazy := func(opts ...Option) *session {
		return serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(connect, in, out, opts...) })
	}

	c := lazy()
	handshake := c.result("initialize", map[string]interface{}{"protocolVersion": "2025-06-18"})
	assert.Equal(t, "2025-06-18", handshake["protocolVersion"])
	assert.Equal(t, "argon", handshake["serverInfo"].(map[string]interface{})["name"])
	c.send(map[string]interface{}{"jsonrpc": "2.0", "method": "notifications/initialized"})
	assert.Empty(t, c.result("ping", nil))
	tools := c.tools()
	require.Len(t, tools, 13)
	assert.Equal(t, listedTools(t, newBareServer()), tools, "titles, annotations and schemas do not depend on a connection")
	assert.NotNil(t, c.call("tools/call", map[string]interface{}{"name": "argon_no_such_tool"})["error"])
	require.NoError(t, c.end())

	readOnly := lazy(ReadOnly())
	var names []string
	for name := range readOnly.tools() {
		names = append(names, name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"argon_branch_list", "argon_diff", "argon_pin_list"}, names)
	assert.NotNil(t, readOnly.call("tools/call", map[string]interface{}{"name": "argon_undo"})["error"], "a withheld tool is refused before any connection")
	require.NoError(t, readOnly.end())

	restricted := lazy(DisableTools("argon_merge_apply"))
	tools = restricted.tools()
	assert.Len(t, tools, 12)
	assert.NotContains(t, tools, "argon_merge_apply")
	require.NoError(t, restricted.end())

	assert.Zero(t, attempts.Load(), "nothing above needs MongoDB")
}

// The first tool call is what needs MongoDB. When it is not there the call
// says so as an ordinary tool error, the server keeps serving, and the next
// call tries again instead of repeating a remembered failure.
func TestAToolCallReportsAnUnreachableDatabaseAndTheServerStaysUp(t *testing.T) {
	t.Setenv("MONGODB_URI", nothingListening)
	var attempts atomic.Int32
	connect := func(ctx context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		return walcli.Connect(ctx)
	}
	c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(connect, in, out) })

	for attempt := 1; attempt <= 3; attempt++ {
		text, failed := c.tool("argon_sandbox_create", map[string]interface{}{"project": "shop"})
		require.True(t, failed, "no tool can succeed without MongoDB: %s", text)
		for _, want := range unreachableWording {
			assert.Contains(t, text, want)
		}
		assert.Empty(t, c.result("ping", nil), "the server answers after a failed call")
		assert.Len(t, c.tools(), 13)
		assert.EqualValues(t, attempt, attempts.Load(), "every call tries to connect again")
	}
	assert.Zero(t, sweepers(), "nothing runs in the background before a connection exists")
	require.NoError(t, c.end(), "a server that never connected has nothing to shut down")
	// A failed attempt closes the client it opened; otherwise every retry
	// would leave monitors probing the dead address.
	require.Eventually(t, func() bool { return !strings.Contains(goroutines(), "go.mongodb.org/mongo-driver/") },
		10*time.Second, 20*time.Millisecond, "failed attempts must not leave driver clients behind")
}

// Anything else that keeps the engine from starting is reported as it is,
// and is no more final than an unreachable database.
func TestAnEngineThatCannotStartIsReportedAndTriedAgain(t *testing.T) {
	var attempts atomic.Int32
	connect := func(context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		return nil, errors.New("failed to create WAL service: not authorized on argon_wal")
	}
	c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(connect, in, out) })
	for attempt := 1; attempt <= 2; attempt++ {
		text, failed := c.tool("argon_branch_list", map[string]interface{}{"project": "shop"})
		require.True(t, failed, text)
		assert.Contains(t, text, "not authorized on argon_wal")
		assert.NotContains(t, text, "not reachable", "MongoDB answered; the advice for an unreachable one would mislead")
		assert.EqualValues(t, attempt, attempts.Load())
	}
	require.NoError(t, c.end())
}

// A deployment that accepts connections and never answers is the slow way to
// be unreachable: left to the driver, the caller waits out its 30-second
// server selection.
func TestAnUnresponsiveDatabaseDoesNotHoldTheCall(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var held struct {
		sync.Mutex
		conns []net.Conn
	}
	t.Cleanup(func() {
		_ = listener.Close()
		held.Lock()
		defer held.Unlock()
		for _, conn := range held.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held.Lock()
			held.conns = append(held.conns, conn)
			held.Unlock()
		}
	}()
	t.Setenv("MONGODB_URI", "mongodb://"+listener.Addr().String())

	c := serve(t, func(in io.Reader, out io.Writer) *Server {
		s := NewLazy(walcli.Connect, in, out)
		s.connectTimeout = 300 * time.Millisecond
		return s
	})
	started := time.Now()
	text, failed := c.tool("argon_pin_list", map[string]interface{}{"project": "shop"})
	require.True(t, failed, text)
	assert.Contains(t, text, "MongoDB is not reachable")
	assert.Less(t, time.Since(started), 10*time.Second, "the attempt ends at the server's bound, not the driver's")
	require.NoError(t, c.end())
}

// engine is a connector that counts its attempts and keeps what the
// successful one opened, so the test can seed data and inspect capture.
type engine struct {
	metadataDB string
	attempts   atomic.Int32
	services   atomic.Pointer[walcli.Services]
}

// newEngine points the engine at a metadata database of its own. Everything
// a connection made through it creates is removed when the test ends.
func newEngine(t *testing.T) *engine {
	t.Helper()
	e := &engine{metadataDB: fmt.Sprintf("argon_mcp_test_%d", time.Now().UnixNano())}
	t.Setenv("ARGON_METADATA_DB", e.metadataDB)
	t.Cleanup(func() {
		services := e.services.Load()
		if services == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		projects, _ := services.Projects.ListProjects()
		for _, project := range projects {
			branches, _ := services.Branches.ListBranches(project.ID)
			for _, branch := range branches {
				if branch.PhysicalDB != "" {
					_ = services.Client.Database(branch.PhysicalDB).Drop(ctx)
				}
			}
		}
		_ = services.Client.Database(e.metadataDB).Drop(ctx)
		services.Monitor.Stop()
		_ = services.Client.Disconnect(ctx)
	})
	return e
}

func (e *engine) connect(ctx context.Context) (*walcli.Services, error) {
	e.attempts.Add(1)
	services, err := walcli.Connect(ctx)
	if err == nil {
		e.services.Store(services)
	}
	return services, err
}

// The server was started before its database. Once MongoDB is there the next
// call connects, and from then on it is the server a client would have got by
// starting it late: the sandboxes it hands out are captured, expired ones are
// swept, and all of that stops when the client goes away.
func TestALaterCallConnectsOnceTheDatabaseIsReachable(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", nothingListening)
	c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(e.connect, in, out) })

	text, failed := c.tool("argon_branch_list", map[string]interface{}{"project": "shop"})
	require.True(t, failed, text)
	assert.Contains(t, text, "MongoDB is not reachable")

	// MongoDB comes up. Nothing restarts the server.
	t.Setenv("MONGODB_URI", replicaSet)
	text, failed = c.tool("argon_branch_list", map[string]interface{}{"project": "shop"})
	services := e.services.Load()
	require.NotNil(t, services, "the first call after MongoDB came up connects: %s", text)
	require.True(t, failed)
	assert.Contains(t, text, `project "shop" not found`, "the engine answered; the project does not exist yet")

	_, err := services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed = c.tool("argon_branch_list", map[string]interface{}{"project": "shop"})
	require.False(t, failed, text)
	assert.Contains(t, text, "main: head LSN")

	// A sandbox handed out after the late connection is captured like any other.
	text, failed = c.tool("argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "run-1"})
	require.False(t, failed, text)
	assert.Contains(t, text, "Connection string:")
	capture := services.Ingest.Statuses()
	require.Len(t, capture, 1)
	assert.Equal(t, "running", capture[0].State)

	assert.EqualValues(t, 2, e.attempts.Load(), "one failed attempt, one connection, and no reconnecting after it")
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond,
		"the sweeper starts with the connection")

	require.NoError(t, c.end())
	capture = services.Ingest.Statuses()
	require.Len(t, capture, 1)
	assert.Equal(t, "stopped", capture[0].State, "capture stops with the server")
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond,
		"the sweeper stops with the server")
}

// Requests are served one at a time, but connecting must not rely on that:
// calls that arrive together share one connection and one sweeper.
func TestCallsArrivingTogetherConnectOnce(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	out := &bytes.Buffer{}
	s := NewLazy(e.connect, bytes.NewReader(nil), out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	params, err := json.Marshal(map[string]interface{}{"name": "argon_branch_list", "arguments": map[string]interface{}{"project": "shop"}})
	require.NoError(t, err)
	const callers = 16
	var wg sync.WaitGroup
	for id := 1; id <= callers; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.dispatch(ctx, &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: "tools/call", Params: params})
		}()
	}
	wg.Wait()

	require.NotNil(t, e.services.Load())
	assert.EqualValues(t, 1, e.attempts.Load())
	assert.Equal(t, callers, strings.Count(out.String(), `project \"shop\" not found`), "every caller reached the engine: %s", out.String())
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond)
	assert.Never(t, func() bool { return sweepers() > 1 }, 300*time.Millisecond, 20*time.Millisecond)

	cancel()
	s.stopAllIngesters()
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)
}
