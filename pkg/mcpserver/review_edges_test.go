package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review: a batch made only of client responses must get 202 with no body
// (2025-03-26 Streamable HTTP: "If the input consists solely of JSON-RPC
// responses or notifications ... MUST return HTTP status code 202 Accepted
// with no body"), as the single-message path already does.
func TestReviewBatchOfClientResponsesIsAccepted(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})

	single := post(t, h, `{"jsonrpc":"2.0","id":7,"result":{}}`, nil)
	assert.Equal(t, http.StatusAccepted, single.Code)

	rec := post(t, h, `[{"jsonrpc":"2.0","id":7,"result":{}}]`, nil)
	assert.Equal(t, http.StatusAccepted, rec.Code, "batch of one response: %s", rec.Body.String())
	assert.Empty(t, rec.Body.String())

	// Mixed: only the request is answered.
	rec = post(t, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"x"}}]`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var replies []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &replies))
	assert.Len(t, replies, 1, "a client response must not be answered: %s", rec.Body.String())
}

// Review: --allow-origin is documented as the browser origins "allowed to
// call the HTTP server", but no CORS headers are ever sent, so a browser
// page on an allowed origin (or on a loopback origin with another port)
// cannot complete a preflight or read a response.
func TestReviewAllowedBrowserOriginCanActuallyCall(t *testing.T) {
	origin := "https://cloud.argonlabs.tech"
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{origin}})

	pre := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	pre.Header.Set("Origin", origin)
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "content-type, mcp-protocol-version")
	preRec := httptest.NewRecorder()
	h.ServeHTTP(preRec, pre)
	assert.Less(t, preRec.Code, 300, "preflight status")
	assert.Equal(t, origin, preRec.Header().Get("Access-Control-Allow-Origin"), "preflight ACAO")

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": origin})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"), "POST ACAO")
}

// Review (nit): bodies that are not a JSON-RPC message are answered 202
// as if they were notifications, instead of 400 / -32600 Invalid Request.
// A request with an id but no method gets 202 too, so the client waits on
// an id that is never answered.
func TestReviewNonMessagesAreNotAccepted(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	for _, body := range []string{`null`, `{}`, `[null]`, `{"jsonrpc":"2.0","id":1}`} {
		rec := post(t, h, body, nil)
		assert.NotEqual(t, http.StatusAccepted, rec.Code, "body %s answered 202", body)
	}
}

// Review (nit): one malformed element sinks the whole batch, so the valid
// ping in it is never answered; JSON-RPC 2.0 answers each element.
func TestReviewBatchWithOneBadElementAnswersTheRest(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"},5]`, nil)
	assert.Contains(t, rec.Body.String(), `"id":1`, "ping in batch not answered: %d %s", rec.Code, rec.Body.String())
}

// Review (nit): an --allow-origin written with the default port, or a
// bearer header with RFC 6750's 1*SP of more than one space, never matches.
func TestReviewOriginAndBearerNormalisation(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{"https://cloud.argonlabs.tech:443"}})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://cloud.argonlabs.tech"})
	assert.Equal(t, http.StatusOK, rec.Code, "default-port allowed origin")

	token := strings.Repeat("k", 32)
	h = NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: token})
	rec = post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Authorization": "Bearer  " + token})
	assert.Equal(t, http.StatusOK, rec.Code, "two spaces after Bearer")
}

// Edge cases that should already hold; kept as regression checks.
func TestReviewEdgesThatHold(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{"https://ok.example/"}})
	ping := func(id string) string { return `{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}` }

	for _, id := range []string{`"abc"`, `1.5e3`, `-0`, `null`, `""`} {
		rec := post(t, h, ping(id), nil)
		require.Equal(t, http.StatusOK, rec.Code, id)
		assert.Contains(t, rec.Body.String(), `"id":`+id, id)
	}
	for _, body := range []string{"\n\t " + ping("1") + "\r\n", "\n [" + ping("1") + "]\n"} {
		assert.Equal(t, http.StatusOK, post(t, h, body, nil).Code, "%q", body)
	}
	for _, body := range []string{"", "   ", "[]", ping("1") + " x", ping("1") + ping("2"), "\xef\xbb\xbf" + ping("1")} {
		assert.Equal(t, http.StatusBadRequest, post(t, h, body, nil).Code, "%q", body)
	}
	// Content-Type and Accept are not enforced.
	assert.Equal(t, http.StatusOK, post(t, h, ping("1"), map[string]string{"Content-Type": "text/plain", "Accept": "*/*"}).Code)
	for v, want := range map[string]int{"2024-11-05": 200, "2025-03-26": 200, "2025-06-18": 200, "2026-07-28": 400, "2025-06-18 ": 400, "garbage": 400} {
		assert.Equal(t, want, post(t, h, ping("1"), map[string]string{"MCP-Protocol-Version": v}).Code, v)
	}
	for origin, want := range map[string]int{
		"http://localhost@evil.com":           403,
		"http://evil.com@localhost":           403, // browsers never send userinfo; refused
		"http://127.0.0.1.evil.com":           403,
		"http://[::1]:3000":                   200,
		"http://[::ffff:127.0.0.1]:80":        200,
		"http://127.0.0.2":                    200,
		"http://0.0.0.0:3000":                 403,
		"http://0x7f000001":                   403,
		"http://2130706433":                   403,
		"http://LOCALHOST:3000":               200, // normalised; browsers lowercase anyway
		"http://localhost.":                   403,
		"ws://localhost":                      403,
		"file://":                             403,
		"https://ok.example":                  200,
		"https://OK.example":                  200,
		"https://ok.example:8443":             403,
		"https://ok.example.evil.com":         403,
		"https://evil.com/https://ok.example": 403,
	} {
		assert.Equal(t, want, post(t, h, ping("1"), map[string]string{"Origin": origin}).Code, origin)
	}
	// Exactly maxHTTPBody is read; one byte more is refused.
	prefix, suffix := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"`, `"}`
	fill := maxHTTPBody - len(prefix) - len(suffix)
	exact := prefix + strings.Repeat("a", fill) + suffix
	assert.Equal(t, http.StatusOK, post(t, h, exact, nil).Code)
	over := prefix + strings.Repeat("a", fill+1) + suffix
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(t, h, over, nil).Code)

	for addr, want := range map[string]bool{
		"[::ffff:127.0.0.1]:1": true, "[::]:1": false, "127.0.0.1": false, "localhost": false,
		"127.1:1": false, "[fe80::1%lo0]:1": false, "::1:1": false, "[]:1": false,
	} {
		assert.Equal(t, want, IsLoopbackAddress(addr), addr)
	}
}

// Paths other than /mcp are not served by ServeHTTP's mux.
func TestReviewOnlyMCPPathIsServed(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{}))
	for path, want := range map[string]int{"/mcp": 200, "/mcp?x=1": 200, "/mcp/": 404, "/MCP": 404, "/": 404, "/mcp/x": 404} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, path)
	}
}
