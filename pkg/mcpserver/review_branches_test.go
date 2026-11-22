package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Streamable HTTP (2025-03-26): "If the input consists solely of (any number
// of) JSON-RPC responses or notifications ... the server MUST return HTTP
// status code 202 Accepted with no body." The single-message path special-
// cases a client response (Method == ""); the batch path does not.
func TestReviewBatchOfOnlyClientResponsesIsAccepted(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `[{"jsonrpc":"2.0","id":7,"result":{}},{"jsonrpc":"2.0","id":8,"error":{"code":-1,"message":"no"}}]`, nil)
	assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String())
}

// JSON-RPC 2.0: a response must not be answered. In a batch mixing a request
// and a client response, only the request gets a reply.
func TestReviewMixedBatchAnswersOnlyTheRequest(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":99,"result":{}}]`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var batch []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &batch), rec.Body.String())
	assert.Len(t, batch, 1, "only the ping is answered: %s", rec.Body.String())
}

// JSON-RPC 2.0 section 6: an empty array is an Invalid Request (-32600),
// not a Parse error (-32700): the body is valid JSON.
func TestReviewEmptyBatchIsAnInvalidRequest(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `[]`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "-32600", rec.Body.String())
}

// JSON-RPC 2.0 section 6: one invalid element does not void the batch; the
// valid requests are still answered.
func TestReviewOneInvalidBatchElementDoesNotDropTheOthers(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"},5]`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"id":1`, "the ping is answered")
}

// A body that is valid JSON but neither a request, a notification nor a
// response is not something the server "accepts": the transport says to
// answer with an HTTP error status (400), not 202.
func TestReviewMessageThatIsNeitherRequestNorResponseIsRefused(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	for _, body := range []string{`{}`, `null`, `{"jsonrpc":"2.0","id":1}`, `"ping"`} {
		rec := post(t, h, body, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body %s answered %d %q", body, rec.Code, rec.Body.String())
	}
}

// Stdio: JSON-RPC 2.0 says a response must not be replied to. The server
// answers a client's response with "method not found" (before and after
// this change).
func TestReviewStdioDoesNotAnswerAClientResponse(t *testing.T) {
	out := &bytes.Buffer{}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":5,"result":{}}` + "\n" + `{"jsonrpc":"2.0","id":6,"method":"ping"}` + "\n")
	s := New(nil, in, out)
	require.NoError(t, s.Run(context.Background()))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	assert.Len(t, lines, 1, "only the ping is answered: %s", out.String())
}

// Stdio equivalence of respond() with the old dispatch: this transcript was
// checked against the parent commit too and gives the same output there.
func TestReviewStdioTranscriptMatchesThePreviousDispatch(t *testing.T) {
	transcript := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"no/such"}`,
		`{"jsonrpc":"2.0","id":3,"method":"no/such"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":"bad"}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"argon_nope"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"argon_merge_apply","arguments":{"plan_id":"x"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"argon_merge_apply","arguments":{"plan_id":"x","strategy":5}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"argon_merge_apply","arguments":{"plan_id":"x","strategy":5}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"argon_branch_list","arguments":{"project":"p"}}}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":"s","method":"ping"}`,
	}
	out := &bytes.Buffer{}
	s := NewLazy(noEngine, strings.NewReader(strings.Join(transcript, "\n")+"\n"), out, DisableTools("argon_merge_apply"))
	require.NoError(t, s.Run(context.Background()))
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var reply map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(line), &reply), line)
		ids = append(ids, string(reply["id"]))
		if string(reply["id"]) == "7" || string(reply["id"]) == "8" {
			assert.Contains(t, line, "disabled", "withheld tool refused first")
		}
		if string(reply["id"]) == "9" {
			assert.Contains(t, line, `"isError":true`)
		}
	}
	assert.Equal(t, []string{"1", "3", "4", "5", "6", "7", "8", "9", "null", `"s"`}, ids, out.String())
}

// HTTPHandler is exported and its tests mount it without ServeHTTP. Then
// s.life is nil, lifetime() falls back to the request context, and the
// sweeper the first tool call starts is bound to that one request: it stops
// as soon as the response is written and, with s.sweeping already true,
// never starts again. Through ServeHTTP the same call keeps the sweeper.
func TestReviewSweeperOutlivesTheRequestThatOpenedTheEngine(t *testing.T) {
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"argon_branch_list","arguments":{"project":"shop"}}}`
	check := func(t *testing.T, url string, e *engine) {
		resp, err := http.Post(url, "application/json", strings.NewReader(call))
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NotNil(t, e.services.Load(), string(body))
		time.Sleep(500 * time.Millisecond)
		assert.Equal(t, 1, sweepers(), "a sweeper runs after the request that opened the engine has ended")
	}
	t.Run("ServeHTTP", func(t *testing.T) {
		e := newEngine(t)
		t.Setenv("MONGODB_URI", replicaSet)
		s := NewLazy(e.connect, nil, nil)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.ServeHTTP(ctx, listener, HTTPConfig{}) }()
		check(t, "http://"+listener.Addr().String()+"/mcp", e)
		cancel()
		require.NoError(t, <-done)
		require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond)
	})
	t.Run("HTTPHandler", func(t *testing.T) {
		e := newEngine(t)
		t.Setenv("MONGODB_URI", replicaSet)
		s := NewLazy(e.connect, nil, nil)
		srv := httptest.NewServer(s.HTTPHandler(HTTPConfig{}))
		defer srv.Close()
		defer func() { s.Close(); require.Eventually(t, func() bool { return sweepers() == 0 }, 10*time.Second, 10*time.Millisecond) }()
		check(t, srv.URL, e)
	})
}
