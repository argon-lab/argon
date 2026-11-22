package mcpserver

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startSlowBody opens a connection to endpoint and sends a POST whose body
// is announced but only partly sent, so the handler sits in its body read.
func startSlowBody(t *testing.T, endpoint string) net.Conn {
	t.Helper()
	addr := strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"jsonrpc\":", addr)
	require.NoError(t, err)
	return conn
}

// Round 2 finding: ServeHTTP's comment says calls in flight get
// shutdownGrace (10s) to finish and are then canceled. A request whose body
// is still arriving is not stopped by canceling its context: the handler
// blocks in the body read until ReadTimeout (one minute after the request
// began), and ServeHTTP waits for it in inFlight.Wait(). So stopping the
// server can take about a minute, not about ten seconds, because one client
// trickles a body.
func TestRound2ShutdownIsBoundedByTheGraceWhileABodyIsArriving(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for up to a minute")
	}
	s := NewLazy(noEngine, nil, nil)
	endpoint, cancel, done := serveHTTPForReview(t, s)
	startSlowBody(t, endpoint)
	time.Sleep(200 * time.Millisecond) // the handler is in its body read

	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
		took := time.Since(stopped)
		assert.Less(t, took, shutdownGrace+5*time.Second, "ServeHTTP returned after %s", took)
	case <-time.After(shutdownGrace + 5*time.Second):
		<-done
		t.Fatalf("ServeHTTP had not returned %s after its context ended (grace %s): a slow request body holds shutdown until ReadTimeout; it returned after %s",
			shutdownGrace+5*time.Second, shutdownGrace, time.Since(stopped).Round(time.Second))
	}
}

// Round 2 finding (nit): a body that does not arrive within ReadTimeout is
// answered 400 "cannot read request body", which tells the client its
// request was malformed. It timed out: 408 Request Timeout is the status.
func TestRound2ABodyThatTimesOutIsAnswered408(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for more than a minute")
	}
	s := NewLazy(noEngine, nil, nil)
	endpoint, _, _ := serveHTTPForReview(t, s)
	conn := startSlowBody(t, endpoint)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(90*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equalf(t, http.StatusRequestTimeout, resp.StatusCode, "body %q", strings.TrimSpace(string(body)))
}
