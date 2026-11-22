package mcpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxHTTPBody bounds one POSTed message, as the stdio reader bounds a line.
const maxHTTPBody = 16 * 1024 * 1024

// HTTPConfig configures the Streamable HTTP transport.
type HTTPConfig struct {
	// Token, when set, must arrive as "Authorization: Bearer <Token>" on
	// every request.
	Token string
	// AllowedOrigins are browser origins allowed besides loopback ones
	// (http://localhost:*, http://127.0.0.1:*, http://[::1]:*). A request
	// without an Origin header is not from a browser and is not checked.
	AllowedOrigins []string
}

// HTTPHandler serves MCP over the Streamable HTTP transport (protocol
// revisions 2025-03-26 and 2025-06-18) at whatever path it is mounted on.
// Each POST carries one JSON-RPC message, or a batch for 2025-03-26 clients,
// and a request is answered with a single application/json body; the server
// offers no server-initiated stream, so GET is refused with 405, which the
// transport allows. The server keeps no sessions: every request stands on
// its own, as the tools already do.
//
// The handler only answers requests. Serve with ServeHTTP, which also runs
// the sweeper, the background connection and capture shutdown.
func (s *Server) HTTPHandler(cfg HTTPConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r.Header.Get("Origin"), cfg.AllowedOrigins) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if cfg.Token != "" && !bearerMatches(r.Header.Get("Authorization"), cfg.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="argon"`)
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "this server answers POST requests only; it offers no server-initiated stream", http.StatusMethodNotAllowed)
			return
		}
		if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedVersion(version) {
			http.Error(w, "unsupported MCP-Protocol-Version "+version, http.StatusBadRequest)
			return
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(http.MaxBytesReader(w, r.Body, maxHTTPBody)); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		raw := bytes.TrimSpace(body.Bytes())

		if len(raw) > 0 && raw[0] == '[' {
			var batch []jsonRPCRequest
			if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 {
				writeJSON(w, http.StatusBadRequest, replyError(json.RawMessage("null"), -32700, "parse error"))
				return
			}
			var replies []*jsonRPCResponse
			for i := range batch {
				if resp := s.respond(r.Context(), &batch[i]); resp != nil {
					replies = append(replies, resp)
				}
			}
			if len(replies) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, http.StatusOK, replies)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, replyError(json.RawMessage("null"), -32700, "parse error"))
			return
		}
		// A notification, or the client's response to a request this server
		// never sends, is accepted with no body.
		resp := s.respond(r.Context(), &req)
		if resp == nil || req.Method == "" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// ServeHTTP serves MCP at /mcp on the listener until ctx ends, then shuts
// the listener down and stops the ingesters this server supervises, as Run
// does when its input closes.
func (s *Server) ServeHTTP(ctx context.Context, listener net.Listener, cfg HTTPConfig) error {
	ctx, stop := s.start(ctx)
	defer stop()
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.HTTPHandler(cfg))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Printf("mcp: http shutdown: %v", err)
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// IsLoopbackAddress reports whether a listen address only accepts
// connections from this machine. An empty host listens everywhere.
func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func supportedVersion(version string) bool {
	for _, v := range supportedProtocolVersions {
		if v == version {
			return true
		}
	}
	return false
}

func bearerMatches(header, token string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(token)) == 1
}

// originAllowed guards against DNS rebinding: a browser page on another
// site must not reach a server listening on this machine.
func originAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimRight(a, "/"), origin) {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	payload, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
