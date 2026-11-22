package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveHTTPForReview runs ServeHTTP on a loopback listener and returns the
// endpoint, a cancel func (SIGTERM) and a channel that yields ServeHTTP's
// return value.
func serveHTTPForReview(t *testing.T, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{}) }()
	t.Cleanup(cancel)
	return "http://" + listener.Addr().String() + "/mcp", cancel, done
}

func reviewToolCall(ctx context.Context, endpoint string, id int, name string, args map[string]interface{}) (string, bool, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]interface{}{"name": name, "arguments": args},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return "", false, fmt.Errorf("status %d: %w", resp.StatusCode, err)
	}
	if len(reply.Result.Content) == 0 {
		return "", false, fmt.Errorf("status %d: no content", resp.StatusCode)
	}
	return reply.Result.Content[0].Text, reply.Result.IsError, nil
}

// registerReviewTool adds a test-only tool and removes it when the test ends.
func registerReviewTool(t *testing.T, name string, h toolHandler) {
	t.Helper()
	toolHandlers[name] = h
	t.Cleanup(func() { delete(toolHandlers, name) })
}

// Coverage, expected to pass: concurrent HTTP tool calls through ServeHTTP
// open the engine once and run one sweeper, and the sweeper stops with
// ServeHTTP.
func TestReviewConcurrentHTTPToolCallsOpenTheEngineOnce(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	endpoint, cancel, done := serveHTTPForReview(t, s)

	const callers = 16
	var wg sync.WaitGroup
	var notFound atomic.Int32
	for id := 1; id <= callers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			text, _, err := reviewToolCall(context.Background(), endpoint, id, "argon_branch_list", map[string]interface{}{"project": "shop"})
			if err == nil && strings.Contains(text, `project "shop" not found`) {
				notFound.Add(1)
			}
		}(id)
	}
	wg.Wait()
	assert.EqualValues(t, 1, e.attempts.Load())
	assert.EqualValues(t, callers, notFound.Load())
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond)
	assert.Never(t, func() bool { return sweepers() != 1 }, 300*time.Millisecond, 20*time.Millisecond,
		"the sweeper lives under the server context, not the request that opened the engine")

	cancel()
	require.NoError(t, <-done)
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)
}

// Finding: while MongoDB is down, N concurrent HTTP tool calls queue on
// engineMu and each makes its own connect attempt, so the Nth caller waits
// about N x connectTimeout, and callers that already gave up (request
// context canceled) still get an attempt of their own, because openEngine
// runs under lifetime() rather than the request context.
func TestReviewConcurrentCallsWhileTheDatabaseIsDownEachWaitOneAttempt(t *testing.T) {
	var attempts atomic.Int32
	connect := func(ctx context.Context) (*walcli.Services, error) {
		attempts.Add(1)
		<-ctx.Done() // an unresponsive deployment: the attempt runs to its timeout
		return nil, fmt.Errorf("%w: %v", walcli.ErrUnreachable, ctx.Err())
	}
	s := NewLazy(connect, nil, nil)
	const timeout = 300 * time.Millisecond
	s.connectTimeout = timeout
	endpoint, cancel, done := serveHTTPForReview(t, s)

	const callers = 4
	var wg sync.WaitGroup
	latencies := make([]time.Duration, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			_, _, _ = reviewToolCall(context.Background(), endpoint, i+1, "argon_branch_list", map[string]interface{}{"project": "shop"})
			latencies[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	slowest := time.Duration(0)
	for _, l := range latencies {
		if l > slowest {
			slowest = l
		}
	}
	assert.Less(t, slowest, 2*timeout,
		"a tool call made while MongoDB is away should be answered within about one connect timeout; latencies %v", latencies)

	// Clients that give up after 350ms: only the first attempt was theirs to wait for.
	attempts.Store(0)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, stop := context.WithTimeout(context.Background(), timeout+50*time.Millisecond)
			defer stop()
			_, _, _ = reviewToolCall(ctx, endpoint, 100+i, "argon_branch_list", map[string]interface{}{"project": "shop"})
		}(i)
	}
	wg.Wait()
	time.Sleep(time.Duration(callers+1) * timeout)
	assert.LessOrEqual(t, attempts.Load(), int32(2),
		"requests whose clients already went away should not each run a connect attempt")

	cancel()
	require.NoError(t, <-done)
}

// Finding: ServeHTTP's Shutdown gives in-flight handlers 10 seconds, then
// ServeHTTP calls stop() (stopAllIngesters) and returns while a handler is
// still running. A tool that then starts capture (as argon_connect and
// argon_sandbox_create do via startIngester, under context.Background)
// leaves an ingester running that nothing will stop. Run never has this
// problem: it dispatches synchronously, so stop() follows the last call.
func TestReviewServeHTTPDoesNotReturnWhileAToolCallIsRunning(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	endpoint, cancel, done := serveHTTPForReview(t, s)

	// A real sandbox, created through the real tool.
	_, _, err := reviewToolCall(context.Background(), endpoint, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	services := e.services.Load()
	require.NotNil(t, services)
	_, err = services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed, err := reviewToolCall(context.Background(), endpoint, 2, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "run-1"})
	require.NoError(t, err)
	require.False(t, failed, text)
	_, branchID, err := s.resolveBranchID("shop", "run-1")
	require.NoError(t, err)
	// The sandbox's capture is stopped so the slow tool below restarts it, as
	// argon_connect would.
	require.NoError(t, s.stopIngester(branchID))

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	// A tool whose work does not watch ctx for a while (resolveBranchID,
	// Merge.Compute and ListBranches take no context), then starts capture.
	registerReviewTool(t, "argon_review_slow_connect", func(ctx context.Context, s *Server, _ map[string]interface{}) (string, error) {
		close(started)
		<-release
		defer close(finished)
		return "capture restarted", s.startIngester(branchID)
	})
	go func() {
		_, _, _ = reviewToolCall(context.Background(), endpoint, 3, "argon_review_slow_connect", nil)
	}()
	<-started

	cancel() // SIGTERM
	var returnedEarly bool
	select {
	case err := <-done:
		require.NoError(t, err)
		returnedEarly = true
	case <-time.After(12 * time.Second):
	}
	close(release)
	<-finished
	if !returnedEarly {
		require.NoError(t, <-done)
	}
	assert.False(t, returnedEarly, "ServeHTTP returned, and stopped capture, while a tool call it accepted was still running")

	var running []string
	for _, st := range services.Ingest.Statuses() {
		if st.State == "running" {
			running = append(running, st.BranchID)
		}
	}
	assert.Empty(t, running, "capture started by an in-flight call outlives ServeHTTP")
	stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	_ = services.Ingest.Shutdown(stopCtx)
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)
}

// Finding (nit; the doc comment says to serve with ServeHTTP): HTTPHandler is
// exported and mountable on any mux, but outside ServeHTTP s.life is nil, so
// the engine is opened under the first request's context. The sweeper and
// resumed capture stop as soon as that request ends, and because sweeping
// stays true they never start again.
func TestReviewHTTPHandlerOnItsOwnKeepsTheSweeperPastTheFirstRequest(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	server := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	defer server.Close()
	defer s.Close()

	text, _, err := reviewToolCall(context.Background(), server.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	require.Contains(t, text, `project "shop" not found`)
	_, _, err = reviewToolCall(context.Background(), server.URL, 2, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, sweepers(), "the sweeper should outlive the request that opened the engine")
	s.Close()
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond, "Close stops the sweeper")
}

// Finding (pre_existing, same in Run): serving a Server a second time after
// the engine was opened starts no sweeper and resumes no capture, because
// sweeping stays true after stop() ended the first sweeper.
func TestReviewServingTwiceRunsTheSweeperAgain(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)

	endpoint, cancel, done := serveHTTPForReview(t, s)
	_, _, err := reviewToolCall(context.Background(), endpoint, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)

	endpoint, cancel, done = serveHTTPForReview(t, s)
	_, _, err = reviewToolCall(context.Background(), endpoint, 2, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	ok := assert.Eventually(t, func() bool { return sweepers() == 1 }, 2*time.Second, 10*time.Millisecond,
		"a server serving again sweeps again")
	cancel()
	require.NoError(t, <-done)
	_ = ok
}
