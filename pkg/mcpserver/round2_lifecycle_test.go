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
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 2, finding: openEngine clears s.attempt and closes attempt.done only
// when the Connector returns. A Connector that panics leaves the attempt in
// place forever: net/http recovers the handler's panic, and every later tool
// call waits on an attempt that will never finish, until its own client
// gives up. Before the shared attempt, the deferred Unlock released engineMu
// and the next call connected afresh.
func TestRound2APanickingConnectorDoesNotWedgeLaterCalls(t *testing.T) {
	var calls atomic.Int32
	connect := func(ctx context.Context) (*walcli.Services, error) {
		if calls.Add(1) == 1 {
			panic("connector bug")
		}
		return nil, errors.New("engine refused to start")
	}
	s := NewLazy(connect, nil, nil)
	server := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	defer server.Close()
	defer s.Close()

	// The panic is recovered and reported as a failed attempt.
	text, failed, err := reviewToolCall(context.Background(), server.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Contains(t, text, "connector panicked")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text, failed, err = reviewToolCall(ctx, server.URL, 2, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err, "a call after a Connector panic should be answered, not wait forever on the dead attempt")
	assert.True(t, failed)
	assert.Contains(t, text, "engine refused to start")
	assert.EqualValues(t, 2, calls.Load(), "the next call connects afresh")
}

// acceptOnce hands out the first connection, then fails Accept with a
// non-temporary error once fail is closed, so http.Server.Serve returns it.
type acceptOnce struct {
	net.Listener
	accepted atomic.Int32
	fail     chan struct{}
}

func (l *acceptOnce) Accept() (net.Conn, error) {
	if l.accepted.Add(1) == 1 {
		return l.Listener.Accept()
	}
	<-l.fail
	return nil, errors.New("accept failed")
}

// Round 2, finding: when server.Serve returns an error, ServeHTTP cancels
// calls, waits for the handlers running at that moment and returns, but it
// never closes the http.Server. Connections already accepted keep being
// served: a keep-alive client's next request is answered after ServeHTTP
// returned and after stop() stopped capture, and its tool call runs against
// the engine. inFlight.Wait therefore does not mean "no more calls".
func TestRound2ServeHTTPReturningAnErrorStopsServingOpenConnections(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := &acceptOnce{Listener: inner, fail: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.ServeHTTP(context.Background(), listener, HTTPConfig{}) }()
	endpoint := "http://" + inner.Addr().String() + "/mcp"

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
	defer client.CloseIdleConnections()
	post := func(id int, method, params string) (*http.Response, bool, error) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
		var reused bool
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, reused, err
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
		return resp, reused, nil
	}

	// Opens the engine on the one accepted connection.
	resp, _, err := post(1, "tools/call", `{"name":"argon_branch_list","arguments":{"project":"shop"}}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	close(listener.fail)
	select {
	case err := <-done:
		require.EqualError(t, err, "accept failed")
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return when Serve failed")
	}

	var ran atomic.Bool
	registerReviewTool(t, "argon_review_after_return", func(ctx context.Context, s *Server, _ map[string]interface{}) (string, error) {
		ran.Store(true)
		return "ran", nil
	})
	resp, reused, err := post(2, "ping", `{}`)
	if err == nil {
		t.Errorf("ServeHTTP returned %q, yet a request on a connection it accepted was answered %d afterwards (reused connection: %v)",
			"accept failed", resp.StatusCode, reused)
	}
	resp, _, err = post(3, "tools/call", `{"name":"argon_review_after_return","arguments":{}}`)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("a tool call was served after ServeHTTP returned: status %d, body %s", resp.StatusCode, body)
	}
	assert.False(t, ran.Load(), "a tool ran after ServeHTTP returned and capture was stopped")
}

// Round 2, finding (regression): after Shutdown's grace runs out ServeHTTP
// cancels the calls context and waits for every handler, but a handler
// reading a request body is not woken by its context. The http.Server's
// connections are never closed, so ServeHTTP waits for ReadTimeout (one
// minute) instead of returning shortly after the grace. Before the fix it
// returned at the end of the grace.
func TestRound2ShutdownIsNotHeldPastTheGraceByAStalledBody(t *testing.T) {
	s := NewLazy(noEngine, nil, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{}) }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// Headers and the first byte of a 1000-byte body, then nothing.
	_, err = io.WriteString(conn, "POST /mcp HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{")
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond) // the handler is now reading the body

	stopped := time.Now()
	cancel() // SIGTERM
	limit := shutdownGrace + 3*time.Second
	select {
	case err := <-done:
		require.NoError(t, err)
		t.Logf("ServeHTTP returned %v after the signal", time.Since(stopped).Round(time.Millisecond))
	case <-time.After(limit):
		t.Errorf("ServeHTTP still running %v after the signal (grace %v): a stalled request body holds shutdown until ReadTimeout", limit, shutdownGrace)
		_ = conn.Close()
		<-done
		t.Logf("ServeHTTP returned only after the client closed, %v after the signal", time.Since(stopped).Round(time.Millisecond))
	}
	_, _ = bufio.NewReader(conn).ReadString('\n')
}

// Round 2, finding (nit, mixed use): Close stops a bare handler's sweeper
// but, unlike stop(), leaves sweeping true, so serving the same Server with
// ServeHTTP afterwards opens no sweeper and resumes no capture.
func TestRound2ServeHTTPAfterCloseSweepsAgain(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)

	bare := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	_, _, err := reviewToolCall(context.Background(), bare.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond)
	bare.Close()
	s.Close()
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)

	endpoint, cancel, done := serveHTTPForReview(t, s)
	_, _, err = reviewToolCall(context.Background(), endpoint, 2, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return sweepers() == 1 }, 2*time.Second, 10*time.Millisecond,
		"ServeHTTP after Close should run the sweeper")
	cancel()
	require.NoError(t, <-done)
}

// Round 2, finding (nit): Close cancels s.detached but never clears it, so a
// handler mounted again after Close (a restarted http.Server, a test that
// remounts) hands every request the canceled context: if the engine was not
// open yet, every later connect attempt starts already canceled and no tool
// call can ever connect.
func TestRound2AHandlerMountedAgainAfterCloseCanStillConnect(t *testing.T) {
	var attempts atomic.Int32
	connect := func(ctx context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("database is still starting")
	}
	s := NewLazy(connect, nil, nil)
	first := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	text, failed, err := reviewToolCall(context.Background(), first.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	require.True(t, failed)
	require.Contains(t, text, "database is still starting")
	first.Close()
	s.Close()

	second := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	defer second.Close()
	defer s.Close()
	text, failed, err = reviewToolCall(context.Background(), second.URL, 2, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Contains(t, text, "database is still starting", "the remounted handler's attempt ran under the context Close canceled")
}
