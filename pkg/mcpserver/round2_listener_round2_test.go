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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failingListener hands out real connections until fail is closed, then
// its Accept returns an error, as a listener does when its socket breaks.
type failingListener struct {
	net.Listener
	fail chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	type accepted struct {
		conn net.Conn
		err  error
	}
	got := make(chan accepted, 1)
	go func() {
		c, err := l.Listener.Accept()
		got <- accepted{c, err}
	}()
	select {
	case a := <-got:
		return a.conn, a.err
	case <-l.fail:
		return nil, errors.New("accept: listener broke")
	}
}

func serveOnFailingListener(t *testing.T) (*failingListener, <-chan error) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { inner.Close() })
	l := &failingListener{Listener: inner, fail: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- NewLazy(noEngine, nil, nil).ServeHTTP(ctx, l, HTTPConfig{}) }()
	return l, done
}

const round2Ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

// Round 2 finding: ServeHTTP counts handlers with a sync.WaitGroup whose
// Add runs in each handler goroutine with nothing ordering it before the
// Wait in ServeHTTP. sync.WaitGroup requires an Add from zero to happen
// before Wait; the race detector reports the violation as a data race as
// soon as Wait is entered while a call is in flight. Here the listener
// breaks while a request is being read, which takes ServeHTTP's
// `case err := <-done` path into inFlight.Wait(). (The same report fires on
// the shutdown path when a call outlives shutdownGrace.)
func TestRound2InFlightWaitIsOrderedAfterTheHandlersAdd(t *testing.T) {
	l, done := serveOnFailingListener(t)
	conn, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	body := round2Ping
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:5])
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond) // the handler is running, in its body read

	close(l.fail) // Serve returns; ServeHTTP waits for the handler
	time.Sleep(200 * time.Millisecond)
	_, err = io.WriteString(conn, body[5:])
	require.NoError(t, err)

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return after its listener broke and the call finished")
	}
}

// Round 2 finding (pre-existing path, made a promise by the fix): when the
// listener breaks, ServeHTTP returns and its deferred stop() stops capture,
// but net/http's Serve does not close connections it already accepted. A
// keep-alive client goes on being served after ServeHTTP has returned, so
// calls run after capture was stopped, contrary to "nothing a call starts
// outlives the server".
func TestRound2NoRequestIsServedAfterServeHTTPReturns(t *testing.T) {
	l, done := serveOnFailingListener(t)
	conn, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	send := func() (*http.Response, error) {
		if _, err := fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(round2Ping), round2Ping); err != nil {
			return nil, err
		}
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		resp, err := http.ReadResponse(reader, nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return resp, err
	}
	resp, err := send()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	close(l.fail)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return after its listener broke")
	}

	resp, err = send()
	if err == nil {
		t.Fatalf("ServeHTTP had returned (capture stopped), yet a request on an existing keep-alive connection was answered %d", resp.StatusCode)
	}
	require.False(t, strings.Contains(err.Error(), "timeout"), "the connection should be closed, not left hanging: %v", err)
}
