package mcpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, finding: Close now resets sweeping so that "a handler mounted
// again, or a later ServeHTTP, starts afresh". ServeHTTP does (start() calls
// startSweeper), but a bare handler mounted again never does: the engine is
// already open, so openEngine returns at once and nothing calls
// startSweeper. The remounted handler runs no sweeper and resumes no capture
// for the sandboxes the first one handed out, which Close had stopped, so
// agent writes to a live sandbox go uncaptured until something drains it.
func TestRound3ARemountedBareHandlerSweepsAndResumesCaptureAfterClose(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	ctx := context.Background()

	first := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	_, _, err := reviewToolCall(ctx, first.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	services := e.services.Load()
	require.NotNil(t, services)
	_, err = services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed, err := reviewToolCall(ctx, first.URL, 2, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "run-1"})
	require.NoError(t, err)
	require.False(t, failed, text)
	branch, err := services.ResolveBranch("shop", "run-1")
	require.NoError(t, err)
	require.True(t, services.Ingest.Running(branch.ID), "the sandbox is captured")
	require.Eventually(t, func() bool { return sweepers() == 1 }, 10*time.Second, 10*time.Millisecond)

	first.Close()
	s.Close()
	require.False(t, services.Ingest.Running(branch.ID), "Close stops capture")
	require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)

	second := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	defer second.Close()
	defer s.Close()
	text, failed, err = reviewToolCall(ctx, second.URL, 3, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	require.False(t, failed, text)
	assert.Eventually(t, func() bool { return sweepers() == 1 }, 3*time.Second, 10*time.Millisecond,
		"the remounted handler should run the sweeper again, as Close's comment promises")
	assert.Eventually(t, func() bool { return services.Ingest.Running(branch.ID) }, 5*time.Second, 20*time.Millisecond,
		"the remounted handler should resume capture for the live sandbox, as ServeHTTP after Close does")
}

// Control for the finding above, expected to pass: the same sequence served
// again with ServeHTTP resumes capture and sweeps.
func TestRound3ServeHTTPAfterCloseResumesCapture(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	ctx := context.Background()

	first := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	_, _, err := reviewToolCall(ctx, first.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	services := e.services.Load()
	require.NotNil(t, services)
	_, err = services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed, err := reviewToolCall(ctx, first.URL, 2, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "run-1"})
	require.NoError(t, err)
	require.False(t, failed, text)
	branch, err := services.ResolveBranch("shop", "run-1")
	require.NoError(t, err)
	first.Close()
	s.Close()
	require.False(t, services.Ingest.Running(branch.ID))

	_, cancel, done := serveHTTPForReview(t, s)
	assert.Eventually(t, func() bool { return sweepers() == 1 }, 3*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool { return services.Ingest.Running(branch.ID) }, 5*time.Second, 20*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}

// Coverage, expected to pass: the callTracker on its own.
func TestRound3CallTrackerHolds(t *testing.T) {
	t.Run("wait before any call returns", func(t *testing.T) {
		var c callTracker
		returned := make(chan struct{})
		go func() { c.wait(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("wait with no call in flight blocked")
		}
	})
	t.Run("close refuses and wait waits for the call that entered", func(t *testing.T) {
		var c callTracker
		require.True(t, c.enter())
		c.close()
		assert.False(t, c.enter(), "a closed tracker refuses new calls")
		returned := make(chan struct{})
		go func() { c.wait(); close(returned) }()
		select {
		case <-returned:
			t.Fatal("wait returned with a call in flight")
		case <-time.After(100 * time.Millisecond):
		}
		c.leave()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("wait did not return once the last call left")
		}
	})
	t.Run("concurrent enter and leave against close and wait", func(t *testing.T) {
		for round := 0; round < 200; round++ {
			var c callTracker
			var inside, admittedAfterWait atomic.Int32
			var waited atomic.Bool
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 20; j++ {
						if !c.enter() {
							return
						}
						if waited.Load() {
							admittedAfterWait.Add(1)
						}
						inside.Add(1)
						runtime.Gosched()
						inside.Add(-1)
						c.leave()
					}
				}()
			}
			runtime.Gosched()
			c.close()
			c.wait()
			waited.Store(true)
			assert.Zero(t, inside.Load(), "wait returned with a call inside")
			wg.Wait()
			assert.Zero(t, admittedAfterWait.Load(), "a call was admitted after wait returned")
		}
	})
}

// Coverage, expected to pass: when the listener breaks while a call runs,
// the call is let finish and answered, later calls on its connection are
// refused, and ServeHTTP returns the listener's error.
func TestRound3ACallInFlightWhenTheListenerBreaksIsAnswered(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := &acceptOnce{Listener: inner, fail: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.ServeHTTP(context.Background(), listener, HTTPConfig{}) }()
	endpoint := "http://" + inner.Addr().String() + "/mcp"

	entered, release := make(chan struct{}), make(chan struct{})
	registerReviewTool(t, "argon_review_round3_block", func(ctx context.Context, s *Server, _ map[string]interface{}) (string, error) {
		close(entered)
		select {
		case <-release:
			return "finished", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	type answer struct {
		text   string
		failed bool
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		text, failed, err := reviewToolCall(context.Background(), endpoint, 1, "argon_review_round3_block", map[string]interface{}{})
		answered <- answer{text, failed, err}
	}()
	<-entered
	close(listener.fail)
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("ServeHTTP returned %v while a call was running", err)
	default:
	}
	close(release)
	got := <-answered
	require.NoError(t, got.err)
	assert.False(t, got.failed, got.text)
	assert.Equal(t, "finished", got.text)
	select {
	case err := <-done:
		require.EqualError(t, err, "accept failed")
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("ServeHTTP did not return after the call finished")
	}
}

var goroutineHeader = regexp.MustCompile(`(?m)^goroutine \d+ \[`)

// breakableListener accepts normally until fail is closed, then fails
// Accept with a non-temporary error; unlike acceptOnce, Close unblocks it,
// as it does a real listener.
type breakableListener struct {
	net.Listener
	fail chan struct{}
}

func (l *breakableListener) Accept() (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	got := make(chan result, 1)
	go func() { c, err := l.Listener.Accept(); got <- result{c, err} }()
	select {
	case r := <-got:
		return r.conn, r.err
	case <-l.fail:
		go func() {
			if r := <-got; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, errors.New("accept failed")
	}
}

// Coverage, expected to pass: nothing ServeHTTP starts outlives it, on
// either exit path, with ConnectInBackground and an engine that never opens.
func TestRound3ServeHTTPLeavesNoGoroutinesOnEitherExit(t *testing.T) {
	connect := func(ctx context.Context) (*walcli.Services, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	settle := func() int {
		var n int
		for i := 0; i < 50; i++ {
			runtime.GC()
			// Count what runtime.Stack shows: NumGoroutine also counts a
			// runtime goroutine the first network use starts.
			n = len(goroutineHeader.FindAllStringIndex(goroutines(), -1))
			time.Sleep(20 * time.Millisecond)
			if len(goroutineHeader.FindAllStringIndex(goroutines(), -1)) == n {
				break
			}
		}
		return n
	}
	exercise := func(t *testing.T, stopIt func(cancel context.CancelFunc, l *breakableListener)) {
		before := settle()
		s := NewLazy(connect, nil, nil, ConnectInBackground())
		inner, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listener := &breakableListener{Listener: inner, fail: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{}) }()
		client := &http.Client{Transport: &http.Transport{}}
		resp, err := client.Post("http://"+inner.Addr().String()+"/mcp", "application/json", strings.NewReader(round2Ping))
		require.NoError(t, err)
		_ = resp.Body.Close()
		stopIt(cancel, listener)
		select {
		case <-done:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Fatal("ServeHTTP did not return")
		}
		client.CloseIdleConnections()
		_ = inner.Close()
		// Polled here rather than in assert.Eventually, whose condition runs
		// on a goroutine of its own.
		after := settle()
		for deadline := time.Now().Add(5 * time.Second); after > before && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
			after = settle()
		}
		if after > before {
			t.Errorf("%d goroutines before, %d after ServeHTTP returned:\n%s", before, after, goroutines())
		}
	}
	t.Run("ctx done", func(t *testing.T) {
		exercise(t, func(cancel context.CancelFunc, _ *breakableListener) { cancel() })
	})
	t.Run("serve error", func(t *testing.T) {
		exercise(t, func(_ context.CancelFunc, l *breakableListener) { close(l.fail) })
	})
}

// Coverage, expected to pass: a Connector that panics answers every caller
// waiting on the same attempt, not only the one that ran it.
func TestRound3APanickingConnectorAnswersEveryWaiter(t *testing.T) {
	gate := make(chan struct{})
	var calls atomic.Int32
	connect := func(ctx context.Context) (*walcli.Services, error) {
		if calls.Add(1) == 1 {
			<-gate
			panic("connector bug")
		}
		return nil, errors.New("second attempt")
	}
	s := NewLazy(connect, nil, nil)
	server := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	defer server.Close()
	defer s.Close()

	const waiters = 5
	texts := make(chan string, waiters)
	for i := 0; i < waiters; i++ {
		go func(id int) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			text, _, err := reviewToolCall(ctx, server.URL, id, "argon_branch_list", map[string]interface{}{"project": "shop"})
			if err != nil {
				text = "error: " + err.Error()
			}
			texts <- text
		}(i + 1)
	}
	time.Sleep(300 * time.Millisecond)
	close(gate)
	for i := 0; i < waiters; i++ {
		select {
		case text := <-texts:
			assert.Contains(t, text, "connector panicked")
		case <-time.After(6 * time.Second):
			t.Fatal("a caller waiting on the panicking attempt was never answered")
		}
	}
	assert.EqualValues(t, 1, calls.Load(), "the waiters shared the one attempt")
}

// Round 3, finding (in-flight versus Close's reset): Close resets sweeping
// only for what had started when it ran. A tool call already connecting
// when Close cancels the detached context can still succeed (the Connector
// finishes its handshake), and openEngine then calls startSweeper under the
// canceled context after Close's reset: sweeping is left true with no
// sweeper running, so a later ServeHTTP never sweeps or resumes capture.
// This is the state round 2 fixed for the ordinary order. An embedder hits
// it when its own http.Server.Shutdown times out with a call still
// connecting and it calls Close, as HTTPHandler's comment says to.
func TestRound3CloseDuringAConnectLeavesNoStaleSweepingFlag(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	connecting := make(chan struct{})
	connect := func(ctx context.Context) (*walcli.Services, error) {
		close(connecting)
		<-ctx.Done() // Close cancels the detached context ...
		time.Sleep(300 * time.Millisecond)
		// ... but the connection the attempt was making completes.
		return e.connect(context.Background())
	}
	s := NewLazy(connect, nil, nil)
	bare := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, _, _ = reviewToolCall(context.Background(), bare.URL, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	}()
	<-connecting
	s.Close()
	<-answered
	bare.Close()
	require.NotNil(t, e.services.Load(), "the in-flight call opened the engine")
	require.Eventually(t, func() bool { return sweepers() == 0 }, 5*time.Second, 10*time.Millisecond)

	_, cancel, done := serveHTTPForReview(t, s)
	assert.Eventually(t, func() bool { return sweepers() == 1 }, 3*time.Second, 10*time.Millisecond,
		"ServeHTTP after Close should run the sweeper; sweeping was left true by a startSweeper that ran after Close's reset")
	cancel()
	require.NoError(t, <-done)
}
