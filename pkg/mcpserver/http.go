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
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				http.Error(w, "the request body did not arrive within the read timeout", http.StatusRequestTimeout)
				return
			}
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		reply, status := s.decode(r.Context(), body.Bytes())
		if reply == nil {
			// Only notifications and the client's responses: accepted.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, status, reply)
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
	// A handler mounted again, or a later ServeHTTP, starts afresh.
	s.engineMu.Lock()
	if s.closeDetached != nil {
		s.detached, s.closeDetached = nil, nil
		s.sweeping = false
	}
	s.engineMu.Unlock()
}

// ServeHTTP serves MCP at /mcp on the listener until ctx ends, then shuts
// the listener down and stops the ingesters this server supervises, as Run
// does when its input closes.
func (s *Server) ServeHTTP(ctx context.Context, listener net.Listener, cfg HTTPConfig) error {
	ctx, stop := s.start(ctx)
	defer stop()
	// Calls in flight when ctx ends get a grace period to finish before
	// their context is canceled and their connections are closed, and
	// capture is stopped only after every one has returned, so nothing a
	// call starts outlives the server.
	calls, cancelCalls := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCalls()
	var tracker callTracker
	handler := s.HTTPHandler(cfg)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tracker.enter() {
			http.Error(w, "the server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer tracker.leave()
		handler.ServeHTTP(w, r)
	}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// The body of one message, at most maxHTTPBody.
		ReadTimeout: time.Minute,
		// An idle keep-alive connection, authenticated or not, is closed.
		IdleTimeout:    10 * time.Second,
		MaxHeaderBytes: MaxHeaderBytes,
		BaseContext:    func(net.Listener) context.Context { return calls },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var served error
	select {
	case served = <-done:
		// The listener broke; connections it accepted must not keep serving.
	case <-ctx.Done():
	}
	tracker.close()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Printf("mcp: http shutdown: %v; closing the connections still open and canceling their calls", err)
		_ = server.Close()
	}
	cancelCalls()
	tracker.wait()
	if served == nil {
		served = <-done
	}
	if served != nil && !errors.Is(served, http.ErrServerClosed) {
		return served
	}
	return nil
}

// MaxHeaderBytes bounds the request headers the HTTP transport reads, the
// bearer token among them.
const MaxHeaderBytes = 64 << 10

// callTracker counts calls in flight. Once closed it refuses new ones, so
// wait cannot miss a call that starts while it waits.
type callTracker struct {
	mu      sync.Mutex
	n       int
	closing bool
	idle    *sync.Cond
}

func (c *callTracker) enter() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	c.n++
	return true
}

func (c *callTracker) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 && c.idle != nil {
		c.idle.Broadcast()
	}
}

func (c *callTracker) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
}

func (c *callTracker) wait() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle == nil {
		c.idle = sync.NewCond(&c.mu)
	}
	for c.n > 0 {
		c.idle.Wait()
	}
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
// and host, no default port, no path, IPv6 in its canonical form. Schemes
// other than http and https (chrome-extension://, vscode-webview://) are
// kept as given, lower-cased. It refuses a wildcard, "null", a value with a
// path, query or user info, a scheme browsers do not send (ftp, ws), and a
// host that is not ASCII: give an
// internationalised name in its punycode form, which is what browsers send.
func NormalizeOrigin(origin string) (string, error) {
	normalized, _, err := normalizeOrigin(origin)
	return normalized, err
}

func normalizeOrigin(origin string) (normalized, host string, err error) {
	refuse := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("%q is not an origin (%s): give scheme and host, e.g. https://console.example.com", origin, why)
	}
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" || strings.EqualFold(trimmed, "null") || strings.Contains(trimmed, "*") {
		return refuse("empty, null or a wildcard")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return refuse("no scheme or host")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return refuse("it has user info, a path, a query or a fragment")
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if !browserOriginSchemes[scheme] {
		return refuse("browsers send http, https or an extension or webview scheme")
	}
	if host == "" {
		return refuse("no host")
	}
	for _, r := range host {
		if r <= ' ' || r >= 0x7f || r == '%' {
			return refuse("the host must be ASCII; give an internationalised name in punycode")
		}
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	hostport := host
	if strings.Contains(hostport, ":") {
		hostport = "[" + hostport + "]"
	}
	if port != "" {
		hostport += ":" + port
	}
	return scheme + "://" + hostport, host, nil
}

// browserOriginSchemes are the schemes a browser puts in an Origin header
// for a page that can make requests: web pages, extensions, webviews and
// app shells.
var browserOriginSchemes = map[string]bool{
	"http": true, "https": true,
	"chrome-extension": true, "moz-extension": true, "safari-web-extension": true,
	"ms-browser-extension": true, "vscode-webview": true, "vscode-file": true,
	// App shells that serve their pages from a custom scheme.
	"tauri": true, "capacitor": true,
}

// originAllowed guards against DNS rebinding: a browser page on another
// site must not reach a server listening on this machine.
func originAllowed(origin string, allowed map[string]bool) bool {
	if origin == "" {
		return true
	}
	normalized, host, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	if allowed[normalized] {
		return true
	}
	if !strings.HasPrefix(normalized, "http://") && !strings.HasPrefix(normalized, "https://") {
		return false
	}
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
