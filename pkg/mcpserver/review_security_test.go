package mcpserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review finding: after one answered request (even a refused, unauthenticated
// one) the connection is kept alive with no idle deadline, because the
// http.Server sets ReadHeaderTimeout but neither IdleTimeout nor ReadTimeout.
// net/http then waits for the next request's first byte with no deadline, so
// any client that can reach the port can hold sockets open forever without a
// token.
func TestReviewUnauthenticatedKeepAliveIsNotHeldForever(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the 10s header timeout")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- NewLazy(noEngine, nil, nil).ServeHTTP(ctx, listener, HTTPConfig{Token: strings.Repeat("k", 32)})
	}()
	defer func() { cancel(); <-served }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// The server's own bound for a silent client is ReadHeaderTimeout (10s).
	// An idle keep-alive connection from a client without the token should not
	// outlive that by much.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(14*time.Second)))
	_, err = reader.ReadByte()
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("an unauthenticated keep-alive connection was still open after 14s idle; the server never closes it")
	}
	assert.ErrorIs(t, err, io.EOF)
}

// Review finding: --allow-origin admits an origin past the DNS-rebinding
// check, but the handler sends no CORS headers and answers the preflight
// (which never carries Authorization) like any other request, so a browser
// page on an allowed origin can neither get past preflight when a token is
// set nor read any response.
func TestReviewAllowedOriginCanActuallyCallFromABrowser(t *testing.T) {
	const origin = "https://cloud.argonlabs.tech"
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: strings.Repeat("k", 32), AllowedOrigins: []string{origin}})

	preflight := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	preflight.Header.Set("Origin", origin)
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, preflight)
	assert.Less(t, rec.Code, 300, "preflight from an allowed origin: status %d body %q", rec.Code, rec.Body.String())
	assert.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"), "preflight Access-Control-Allow-Origin")

	post := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{
		"Origin":        origin,
		"Authorization": "Bearer " + strings.Repeat("k", 32),
	})
	require.Equal(t, http.StatusOK, post.Code)
	assert.Equal(t, origin, post.Header().Get("Access-Control-Allow-Origin"), "a browser discards a cross-origin response without this header")
}

// Probes that pass on the current code; they back the "covered by review"
// rows of the case table.
func TestReviewProbesThatHold(t *testing.T) {
	token := strings.Repeat("k", 32)
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: token})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	for auth, want := range map[string]int{
		"bearer " + token:       http.StatusOK, // scheme is case-insensitive (RFC 7235)
		"BEARER " + token:       http.StatusOK,
		"Bearer  " + token:      http.StatusOK, // RFC 6750: 1*SP
		"Bearer\t" + token:      http.StatusUnauthorized,
		"Bearer " + token + "x": http.StatusUnauthorized,
		"Bearer " + token[:31]:  http.StatusUnauthorized,
		"Bearer K" + token[1:]:  http.StatusUnauthorized, // the token itself is case-sensitive
	} {
		rec := post(t, h, ping, map[string]string{"Authorization": auth})
		assert.Equal(t, want, rec.Code, "Authorization %q", auth)
		assert.NotContains(t, rec.Body.String(), token)
	}

	// Unauthenticated requests get 401 whatever the method, version header or
	// body; nothing past the token check runs.
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodOptions, http.MethodPut} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/mcp", nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, m)
		assert.Empty(t, rec.Header().Get("Allow"), m)
	}
	rec := post(t, h, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Origin forms a browser can send.
	for origin, want := range map[string]bool{
		"http://[::1]:3000":            true,
		"http://[::ffff:127.0.0.1]:80": true,
		"http://127.0.0.5":             true,
		"http://0.0.0.0:3000":          false,
		"http://127.0.0.1.nip.io":      false,
		"http://localhost.":            false,
		"http://evil.com#@localhost":   false,
		"http://localhost@evil.com":    false,
		"file://":                      false,
		"chrome-extension://abc":       false,
		"HTTP://EVIL.COM":              false,
		"ws://localhost:1":             false,
	} {
		assert.Equal(t, want, originAllowed(origin, nil), origin)
	}

	// Listen address forms.
	for addr, want := range map[string]bool{
		"[::ffff:127.0.0.1]:1": true,
		"127.255.255.254:1":    true,
		"[::ffff:0.0.0.0]:1":   false,
		"[::]:1":               false,
		"[::1%lo0]:1":          false,
		"LOCALHOST:1":          true,
		"localhost.:1":         false,
		"127.1:1":              false,
		" 127.0.0.1:1":         false,
		"8765":                 false,
		"":                     false,
	} {
		assert.Equal(t, want, IsLoopbackAddress(addr), addr)
	}
	// What IsLoopbackAddress accepts really binds to loopback only.
	for _, addr := range []string{"localhost:0", "[::ffff:127.0.0.1]:0", "127.0.0.1:0", "[::1]:0"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		ip := l.Addr().(*net.TCPAddr).IP
		l.Close()
		assert.True(t, ip.IsLoopback(), "%s bound %s", addr, ip)
	}

	// Body limit holds for a chunked body over a real connection, and the
	// limit is applied only after the token check.
	server := httptest.NewServer(h)
	defer server.Close()
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":"`))
		chunk := []byte(strings.Repeat("a", 1<<20))
		for i := 0; i < 17; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_, _ = pw.Write([]byte(`"}`))
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, server.URL, pr)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		resp.Body.Close()
	}
	pr.CloseWithError(os.ErrClosed)
}
