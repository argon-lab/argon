// Package mcpserver exposes Argon to AI agents over the Model Context
// Protocol (stdio, JSON-RPC 2.0, newline-delimited). The tool surface is
// the agent loop end to end: fork a TTL sandbox and get a connection
// string, work through any MongoDB driver, diff/merge the result back or
// undo it, and let the TTL reclaim whatever is left.
//
// The server supervises a change-stream ingester for every sandbox it
// creates or connects, so agent writes become versioned history without
// anyone running "argon watch" by hand. Protocol traffic owns stdout;
// logs go to stderr.
//
// A server made with NewLazy needs no database to start. It answers the
// handshake and lists its tools with nothing behind it, and opens the engine
// when a tool is first called.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/argon-lab/argon/v2/pkg/version"
	"github.com/argon-lab/argon/v2/pkg/walcli"
)

// supportedProtocolVersions are the handshake-based protocol revisions this
// server speaks, oldest first. For a stdio server that only offers tools
// they differ in what a tool may declare (annotations and titles arrived
// after the first), not in how requests flow. The server answers with the
// client's revision when it is one of these, and with the newest otherwise,
// as the lifecycle rules of those revisions require.
var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

func negotiateProtocolVersion(requested string) string {
	for _, version := range supportedProtocolVersions {
		if version == requested {
			return version
		}
	}
	return supportedProtocolVersions[len(supportedProtocolVersions)-1]
}

// Option configures a Server.
type Option func(*Server)

// ReadOnly limits the server to tools that only inspect state: nothing is
// forked, checked out, merged, undone or deleted through it.
func ReadOnly() Option { return func(s *Server) { s.readOnly = true } }

// ConnectInBackground makes a server created with NewLazy start reaching for
// the database as soon as it runs, without waiting for a tool call. Listing
// tools still needs no database. Once one answers, the server sweeps expired
// sandboxes as a server that connected at startup does.
func ConnectInBackground() Option { return func(s *Server) { s.eager = true } }

// DisableTools withholds the named tools. The usual use is keeping
// argon_merge_apply away from an agent so that applying a reviewed plan
// stays a human action (argon merge apply <plan-id>).
func DisableTools(names ...string) Option {
	return func(s *Server) {
		for _, name := range names {
			if name != "" {
				s.disabled[name] = true
			}
		}
	}
}

// UnknownTools returns the names in the list that are not tools of this
// server, so a misspelled restriction is reported instead of ignored.
func UnknownTools(names []string) []string {
	var unknown []string
	for _, name := range names {
		if _, ok := toolHandlers[name]; !ok && name != "" {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// Connector opens the engine for a server made with NewLazy. The server calls
// it when a tool is called and no engine is open yet: on the first call, and
// on each later one for as long as it fails. Calls never overlap. ctx ends
// when the server stops waiting for the attempt. An error that wraps
// walcli.ErrUnreachable is reported to the agent as a MongoDB deployment
// that could not be reached, any other error in its own words.
type Connector func(ctx context.Context) (*walcli.Services, error)

// defaultConnectTimeout bounds one attempt to open the engine, so a tool
// call made while MongoDB is away is answered well inside a client's request
// timeout.
const defaultConnectTimeout = 10 * time.Second

// Server speaks MCP over a reader/writer pair (stdin/stdout in production).
type Server struct {
	// The engine. New supplies it; NewLazy leaves it nil until a tool call
	// opens it through connect. It is set once, under engineMu, and never
	// replaced. Tool handlers run only after openEngine has succeeded, so
	// they read the field directly.
	engineMu       sync.Mutex
	services       *walcli.Services
	connect        Connector
	connectTimeout time.Duration
	// eager makes a lazy server connect in the background from the start.
	eager bool
	// reconnectDelay is the first wait between background attempts to reach
	// the database; it doubles up to thirty seconds. Zero means one second.
	reconnectDelay time.Duration
	sweeping       bool
	// background counts what the engine's opening started that must finish
	// before ingesters are stopped.
	background sync.WaitGroup

	in    io.Reader
	outMu sync.Mutex
	out   io.Writer

	// Ingesters supervised for sandboxes created/connected via this server.
	ingestMu sync.Mutex
	ingest   map[string]context.CancelFunc

	// Which tools this instance offers; fixed at construction.
	readOnly bool
	disabled map[string]bool
}

// New creates an MCP server over the given transport.
func New(services *walcli.Services, in io.Reader, out io.Writer, opts ...Option) *Server {
	s := &Server{
		services:       services,
		connectTimeout: defaultConnectTimeout,
		in:             in,
		out:            out,
		ingest:         make(map[string]context.CancelFunc),
		disabled:       make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NewLazy creates an MCP server that opens its engine on first use, for a
// process that may be started where no database is running. The handshake,
// ping and tools/list never call connect; the first tools/call does. If that
// fails the call is answered with a tool error, the server keeps serving, and
// the next call tries again.
func NewLazy(connect Connector, in io.Reader, out io.Writer, opts ...Option) *Server {
	s := New(nil, in, out, opts...)
	s.connect = connect
	return s
}

// openEngine makes sure the engine is open, connecting if no earlier call
// has. ctx is the context Run serves under: what starts here stops when Run
// returns.
func (s *Server) openEngine(ctx context.Context) error {
	s.engineMu.Lock()
	defer s.engineMu.Unlock()
	if s.services != nil {
		return nil
	}
	if s.connect == nil {
		return errors.New("no engine was configured for this server")
	}
	attempt, cancel := context.WithTimeout(ctx, s.connectTimeout)
	defer cancel()
	services, err := s.connect(attempt)
	if err != nil {
		// Nothing is remembered: the next call connects afresh.
		return err
	}
	// From here on this is the server New would have built.
	s.services = services
	s.startSweeper(ctx)
	return nil
}

// connectInBackground opens the engine as soon as the database answers. A
// server that connected only when a tool was called would, after a restart
// with no tool call, leave expired sandboxes unswept and their capture
// unsupervised, which the server never did when it connected at startup.
// Listing tools still needs no database: until one answers, this only waits.
func (s *Server) connectInBackground(ctx context.Context) {
	delay := s.reconnectDelay
	if delay <= 0 {
		delay = time.Second
	}
	for {
		if err := s.openEngine(ctx); err == nil {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

// startSweeper reaps expired sandboxes until ctx ends, and resumes capture
// for the branches an earlier process left checked out. It starts once per
// server, at whichever comes later: Run, or the call that opens the engine.
// Callers hold engineMu.
func (s *Server) startSweeper(ctx context.Context) {
	if s.services == nil || s.sweeping {
		return
	}
	s.sweeping = true
	go s.services.RunSandboxSweeper(ctx)
	s.background.Add(1)
	go func() { defer s.background.Done(); s.resumeCapture(ctx, s.services) }()
}

// resumeCapture re-attaches an ingester to every branch that was checked out
// when an earlier process exited. Supervision lives in memory, so without
// this a restarted server would leave those branches uncaptured until a tool
// call happened to drain one, and a write to a collection the agent created
// could by then be outside MongoDB's snapshot history. Capture resumes from
// the stored resume token and keeps the stored actor, so writes made while
// no server ran are caught up rather than lost.
func (s *Server) resumeCapture(ctx context.Context, services *walcli.Services) {
	projects, err := services.Projects.ListProjects()
	if err != nil {
		log.Printf("mcp: cannot list projects to resume capture: %v", err)
		return
	}
	for _, project := range projects {
		branches, err := services.Branches.ListBranches(project.ID)
		if err != nil {
			log.Printf("mcp: cannot list branches of %s to resume capture: %v", project.Name, err)
			continue
		}
		for _, branch := range branches {
			if ctx.Err() != nil {
				return
			}
			if !branch.IsLive() {
				continue
			}
			if err := s.startIngester(branch.ID); err != nil {
				log.Printf("mcp: capture for %s/%s did not resume: %v", project.Name, branch.Name, err)
			}
		}
	}
}

// unavailable is what an agent is told when a tool did not run because the
// engine could not be opened. The server itself is fine, so the text says
// what a person has to put right and that calling again is enough after
// that. For an unreachable deployment the driver's own error stays in the
// log: it can quote the connection string.
func unavailable(err error) string {
	if errors.Is(err, walcli.ErrUnreachable) {
		return "MongoDB is not reachable, so this tool did not run. " +
			"Argon needs a MongoDB replica set (a single-node one is enough) at the address in the " +
			"MONGODB_URI environment variable this server was started with; when that is not set it " +
			"uses mongodb://localhost:27017. Start MongoDB or correct MONGODB_URI, then call the tool " +
			"again: the server is still running and connects on the next call. " +
			"The driver's error is in this server's log, and \"argon doctor\" checks the deployment."
	}
	return fmt.Sprintf("Argon could not start its engine, so this tool did not run: %v. "+
		"The server is still running and tries again on the next call.", err)
}

// offers reports whether a tool is available on this instance.
func (s *Server) offers(name string) bool {
	if s.disabled[name] {
		return false
	}
	return !s.readOnly || toolTraits[name].readOnly
}

// jsonRPCRequest is an incoming JSON-RPC 2.0 message.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// Run serves until the input closes or the context is canceled. Sandbox
// ingesters started by this server stop with it.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); s.background.Wait(); s.stopAllIngesters() }()
	s.engineMu.Lock()
	s.startSweeper(ctx)
	s.engineMu.Unlock()
	if s.connect != nil && s.eager {
		background := make(chan struct{})
		go func() { defer close(background); s.connectInBackground(ctx) }()
		defer func() { cancel(); <-background }()
	}

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lines := make(chan []byte)
	readDone := make(chan error, 1)
	go func() {
		defer func() { readDone <- scanner.Err(); close(lines) }()
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return nil
		case next, ok := <-lines:
			if !ok {
				return <-readDone
			}
			line = next
		}
		if len(line) == 0 {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("mcp: dropping unparseable message: %v", err)
			continue
		}
		s.dispatch(ctx, &req)
	}
}

func (s *Server) dispatch(ctx context.Context, req *jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.reply(req.ID, map[string]interface{}{
			"protocolVersion": negotiateProtocolVersion(params.ProtocolVersion),
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo": map[string]interface{}{
				"name":    "argon",
				"version": version.String(),
			},
		})
	case "notifications/initialized", "notifications/cancelled":
		// Notifications get no response.
	case "ping":
		s.reply(req.ID, map[string]interface{}{})
	case "tools/list":
		s.reply(req.ID, map[string]interface{}{"tools": s.offeredTools()})
	case "tools/call":
		s.handleToolCall(ctx, req)
	default:
		if len(req.ID) != 0 {
			s.replyError(req.ID, -32601, fmt.Sprintf("method %q not found", req.Method))
		}
	}
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

func (s *Server) handleToolCall(ctx context.Context, req *jsonRPCRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.replyError(req.ID, -32602, fmt.Sprintf("invalid tools/call params: %v", err))
		return
	}

	handler, ok := toolHandlers[params.Name]
	if !ok {
		s.replyError(req.ID, -32602, fmt.Sprintf("unknown tool %q", params.Name))
		return
	}
	// A withheld tool is refused even if the client calls it without
	// having seen it listed.
	if !s.offers(params.Name) {
		s.replyError(req.ID, -32602, fmt.Sprintf("tool %q is not available: this server was started with it disabled", params.Name))
		return
	}

	// Every tool works on the engine; nothing before this point does.
	if err := s.openEngine(ctx); err != nil {
		log.Printf("mcp: %s did not run: %v", params.Name, err)
		s.replyToolError(req.ID, unavailable(err))
		return
	}

	text, err := handler(ctx, s, params.Arguments)
	if err != nil {
		s.replyToolError(req.ID, err.Error())
		return
	}
	s.reply(req.ID, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	})
}

// replyToolError answers a tool call that failed. Tool-level failures are
// results with isError, not protocol errors.
func (s *Server) replyToolError(id json.RawMessage, text string) {
	s.reply(id, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
		"isError": true,
	})
}

func (s *Server) reply(id json.RawMessage, result interface{}) {
	if len(id) == 0 {
		return
	}
	s.write(&jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyError(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		return
	}
	s.write(&jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}})
}

func (s *Server) write(resp *jsonRPCResponse) {
	payload, err := json.Marshal(resp)
	if err != nil {
		log.Printf("mcp: failed to encode response: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(payload, '\n'))
}

// startIngester supervises a change-stream ingester for a branch until the
// server stops or the branch is discarded.
func (s *Server) startIngester(branchID string, actors ...string) error {
	actor := ""
	if len(actors) > 0 {
		actor = actors[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.services.StartCapture(ctx, branchID, actor); err != nil {
		return err
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	s.ingest[branchID] = func() { _ = s.stopIngester(branchID) }
	return nil
}

func (s *Server) stopIngester(branchID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.services.Ingest.Stop(ctx, branchID); err != nil {
		return err
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	delete(s.ingest, branchID)
	return nil
}

func (s *Server) stopAllIngesters() {
	s.engineMu.Lock()
	services := s.services
	s.engineMu.Unlock()
	if services == nil {
		// The engine was never opened, so nothing was started.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := services.Ingest.Shutdown(ctx); err != nil {
		log.Printf("mcp: capture shutdown: %v", err)
	}
	snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer snapshotCancel()
	if err := services.WaitAuto(snapshotCtx); err != nil {
		log.Printf("mcp: snapshot shutdown: %v", err)
		// Cancellation interrupts cooperative storage I/O; give its independent
		// deferred publication-lock cleanup a short grace before process exit.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if cleanupErr := services.WaitAuto(cleanupCtx); cleanupErr != nil {
			log.Printf("mcp: snapshot cleanup still incomplete: %v", cleanupErr)
		}
	}

}
