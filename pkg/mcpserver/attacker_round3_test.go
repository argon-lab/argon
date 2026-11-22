package mcpserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3 coverage, expected to pass: the branch 7471d41 added for a request
// that arrives once shutdown has begun (callTracker refuses it) had no test.
// A request whose headers finish after the serving context ended is not
// served, with or without the token, and does not reach the engine.
// ServeHTTP still returns well inside the grace.
func TestRound3ARequestArrivingDuringShutdownIsRefusedWithoutEngineWork(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	for _, auth := range []string{"", "Authorization: Bearer " + token + "\r\n"} {
		var attempts atomic.Int32
		connect := func(context.Context) (*walcli.Services, error) {
			attempts.Add(1)
			return nil, errors.New("no engine")
		}
		s := NewLazy(connect, nil, nil)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{Token: token}) }()

		conn, err := net.Dial("tcp", listener.Addr().String())
		require.NoError(t, err)
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"argon_branch_list","arguments":{"project":"shop"}}}`
		_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\n")
		require.NoError(t, err)
		time.Sleep(200 * time.Millisecond)

		stopped := time.Now()
		cancel()
		time.Sleep(200 * time.Millisecond)
		_, err = fmt.Fprintf(conn, "%sContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", auth, len(body), body)
		require.NoError(t, err)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		// net/http itself drops a request whose headers it finished reading
		// after Shutdown began, closing the connection unanswered; the 503
		// covers the instant between callTracker.close and Shutdown.
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "auth header %q", strings.TrimSpace(auth))
		} else {
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "auth header %q", strings.TrimSpace(auth))
		}
		_ = conn.Close()
		assert.Zero(t, attempts.Load(), "a request refused during shutdown reached the engine")

		select {
		case err := <-done:
			require.NoError(t, err)
			assert.Less(t, time.Since(stopped), shutdownGrace)
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Fatal("ServeHTTP did not return")
		}
	}
}

// Round 3 coverage, expected to pass: an unauthenticated client on a
// token-protected listener gets no 2xx except the CORS preflight of an
// allowed (here loopback) origin, which carries nothing, and never reaches
// the engine, whatever method, path, origin or Authorization it tries.
func TestRound3NoUnauthenticatedRequestGetsAnAnswerOrEngineWork(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	var attempts atomic.Int32
	s := NewLazy(func(context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		return nil, errors.New("no engine")
	}, nil, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{Token: token}) }()
	defer func() { cancel(); <-done }()
	base := "http://" + listener.Addr().String()
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"argon_branch_list","arguments":{"project":"shop"}}}`

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, "PATCH", "PROPFIND"} {
		for _, path := range []string{"/mcp", "/mcp/", "//mcp", "/MCP", "/mcp/../mcp", "/%6dcp", "/"} {
			for _, origin := range []string{"", "http://localhost", "http://127.0.0.1:3000", "https://evil.example", "null", "chrome-extension://x"} {
				for _, authz := range []string{"", "Bearer", "Bearer ", "Bearer " + token[:31], "Bearer " + token + "x", "Basic " + token, "bearer\t" + token, "Bearer " + strings.ToUpper(token)} {
					req, err := http.NewRequest(method, base+path, strings.NewReader(call))
					require.NoError(t, err)
					if origin != "" {
						req.Header.Set("Origin", origin)
					}
					if authz != "" {
						req.Header.Set("Authorization", authz)
					}
					client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
					resp, err := client.Do(req)
					require.NoError(t, err)
					raw, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					preflight := method == http.MethodOptions && (path == "/mcp" || path == "/%6dcp") && resp.StatusCode == http.StatusNoContent &&
						(origin == "http://localhost" || origin == "http://127.0.0.1:3000")
					if resp.StatusCode < 300 && !preflight {
						t.Errorf("%s %s origin %q authorization %q: %d %q without the token", method, path, origin, authz, resp.StatusCode, raw)
					}
					if preflight {
						assert.Empty(t, raw)
					}
				}
			}
		}
	}
	assert.Zero(t, attempts.Load(), "an unauthenticated request reached the engine")
}
