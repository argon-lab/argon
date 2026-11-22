package mcpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
// Serve with ServeHTTP, which also runs the sweeper, the background
// connection and capture shutdown. A handler mounted on a server of the
// caller's own keeps the engine open past the request that opened it; call
// Close when that server stops.
func (s *Server) HTTPHandler(cfg HTTPConfig) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		if normalized, err := NormalizeOrigin(origin); err == nil {
			allowed[normalized] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), detachedLifeKey{}, s.detachedLife()))
		origin := r.Header.Get("Origin")
		if !originAllowed(origin, allowed) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if origin != "" {
			// An allowed browser page may read the answer.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")
			if r.Method == http.MethodOptions {
				// A CORS preflight carries no credentials, so it is answered
				// before the token check.
				w.Header().Set("Access-Control-Allow-Methods", "POST")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
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
		if !json.Valid(raw) {
			writeJSON(w, http.StatusBadRequest, replyError(json.RawMessage("null"), -32700, "parse error"))
			return
		}

		if raw[0] == '[' {
			var batch []json.RawMessage
			if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 {
				writeJSON(w, http.StatusBadRequest, replyError(json.RawMessage("null"), -32600, "invalid request: empty batch"))
				return
			}
			// JSON-RPC answers each element on its own: a bad one gets its
			// own error and does not keep the others from being answered.
			var replies []*jsonRPCResponse
			for _, element := range batch {
				var req jsonRPCRequest
				var resp *jsonRPCResponse
				if err := json.Unmarshal(element, &req); err != nil {
					resp = invalidRequest(nil)
				} else {
					resp = s.respond(r.Context(), &req)
				}
				if resp != nil {
					replies = append(replies, resp)
				}
			}
			if len(replies) == 0 {
				// Only notifications and responses: accepted, no body.
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, http.StatusOK, replies)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(raw, &req); err != nil || !req.isMessage() {
			writeJSON(w, http.StatusBadRequest, invalidRequest(req.ID))
			return
		}
		resp := s.respond(r.Context(), &req)
		if resp == nil {
			// A notification, or the client's response to a request.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// detachedLife is the context a handler mounted without ServeHTTP opens the
// engine under: it lasts until Close.
func (s *Server) detachedLife() context.Context {
	s.engineMu.Lock()
	defer s.engineMu.Unlock()
	if s.detached == nil {
		s.detached, s.closeDetached = context.WithCancel(context.Background())
	}
	return s.detached
}

// Close stops what a handler mounted without ServeHTTP started: the sweeper,
// resumed capture and the ingesters of sandboxes handed out. ServeHTTP and
// Run do this themselves when they return.
func (s *Server) Close() {
	s.engineMu.Lock()
	cancel := s.closeDetached
	s.engineMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.background.Wait()
	s.stopAllIngesters()
}

// ServeHTTP serves MCP at /mcp on the listener until ctx ends, then shuts
// the listener down and stops the ingesters this server supervises, as Run
// does when its input closes.
func (s *Server) ServeHTTP(ctx context.Context, listener net.Listener, cfg HTTPConfig) error {
	ctx, stop := s.start(ctx)
	defer stop()
	// Calls in flight when ctx ends get a grace period to finish before
	// their context is canceled, and capture is stopped only after every
	// one has returned, so nothing a call starts outlives the server.
	calls, cancelCalls := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCalls()
	var inFlight sync.WaitGroup
	handler := s.HTTPHandler(cfg)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Done()
		handler.ServeHTTP(w, r)
	}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// The body of one message, at most maxHTTPBody.
		ReadTimeout: time.Minute,
		// An idle keep-alive connection, authenticated or not, is closed.
		IdleTimeout:    10 * time.Second,
		MaxHeaderBytes: 64 << 10,
		BaseContext:    func(net.Listener) context.Context { return calls },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		cancelCalls()
		inFlight.Wait()
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Printf("mcp: http shutdown: %v; canceling the calls still running", err)
	}
	cancelCalls()
	inFlight.Wait()
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdownGrace is how long calls in flight may run on after the server is
// told to stop, before their context is canceled.
const shutdownGrace = 10 * time.Second

// IsLoopbackAddress reports whether a listen address only accepts
// connections from this machine. An empty host listens everywhere.
func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
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
	const scheme = "Bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	// RFC 6750 allows one or more spaces after the scheme.
	presented := strings.TrimLeft(header[len(scheme):], " ")
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// NormalizeOrigin returns an origin as browsers send it: lower-case scheme
// and host, no default port, no path. It refuses anything that is not a
// plain http or https origin, such as a wildcard or a value with a path.
func NormalizeOrigin(origin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(origin, "*") {
		return "", fmt.Errorf("%q is not an origin: give scheme and host, e.g. https://console.example.com", origin)
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// originAllowed guards against DNS rebinding: a browser page on another
// site must not reach a server listening on this machine.
func originAllowed(origin string, allowed map[string]bool) bool {
	if origin == "" {
		return true
	}
	normalized, err := NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	if allowed[normalized] {
		return true
	}
	u, _ := url.Parse(normalized)
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
