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
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
)

const protocolVersion = "2024-11-05"

// Server speaks MCP over a reader/writer pair (stdin/stdout in production).
type Server struct {
	services *walcli.Services
	in       io.Reader
	outMu    sync.Mutex
	out      io.Writer

	// Ingesters supervised for sandboxes created/connected via this server.
	ingestMu sync.Mutex
	ingest   map[string]context.CancelFunc
}

// New creates an MCP server over the given transport.
func New(services *walcli.Services, in io.Reader, out io.Writer) *Server {
	return &Server{
		services: services,
		in:       in,
		out:      out,
		ingest:   make(map[string]context.CancelFunc),
	}
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
	defer func() { cancel(); s.stopAllIngesters() }()
	go s.services.RunSandboxSweeper(ctx)

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
		s.reply(req.ID, map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo": map[string]interface{}{
				"name":    "argon",
				"version": "2.0",
			},
		})
	case "notifications/initialized", "notifications/cancelled":
		// Notifications get no response.
	case "ping":
		s.reply(req.ID, map[string]interface{}{})
	case "tools/list":
		s.reply(req.ID, map[string]interface{}{"tools": toolDescriptors()})
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

	text, err := handler(ctx, s, params.Arguments)
	if err != nil {
		// Tool-level failures are results with isError, not protocol errors.
		s.reply(req.ID, map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
		return
	}
	s.reply(req.ID, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.services.Ingest.Shutdown(ctx); err != nil {
		log.Printf("mcp: capture shutdown: %v", err)
	}
	snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer snapshotCancel()
	if err := s.services.WaitAuto(snapshotCtx); err != nil {
		log.Printf("mcp: snapshot shutdown: %v", err)
		// Cancellation interrupts cooperative storage I/O; give its independent
		// deferred publication-lock cleanup a short grace before process exit.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if cleanupErr := s.services.WaitAuto(cleanupCtx); cleanupErr != nil {
			log.Printf("mcp: snapshot cleanup still incomplete: %v", cleanupErr)
		}
	}

}
