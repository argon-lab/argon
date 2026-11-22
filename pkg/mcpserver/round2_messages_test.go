package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// servePanics reports the panic a handler raised for the request, if any.
func servePanics(h http.Handler, req *http.Request) (rec *httptest.ResponseRecorder, panicked interface{}) {
	rec = httptest.NewRecorder()
	defer func() { panicked = recover() }()
	h.ServeHTTP(rec, req)
	return rec, nil
}

// Round 2: originAllowed re-parses the normalised origin and ignores the
// error (u, _ := url.Parse(normalized); u.Hostname()). NormalizeOrigin
// builds the origin from u.Hostname(), which is the unescaped host, so an
// Origin whose host holds %25 (an escaped "%", e.g. an IPv6 zone) yields a
// string url.Parse refuses, u is nil and the handler panics with a nil
// dereference before the token check. Before the fix, originAllowed checked
// the url.Parse error and answered 403.
func TestRound2OriginWithEscapedPercentIsRefusedWithoutPanic(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: strings.Repeat("k", 32)})
	for _, origin := range []string{"http://a%25b", "http://[fe80::1%25en0]:3000", "http://127.0.0.1%25.evil.com"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(round2Ping))
		req.Header.Set("Origin", origin)
		rec, panicked := servePanics(h, req)
		if panicked != nil {
			t.Errorf("Origin %q: handler panicked (unauthenticated): %v", origin, panicked)
			continue
		}
		assert.Equal(t, http.StatusForbidden, rec.Code, origin)
	}
}

// The same through a real listener: the client gets a reset connection
// instead of a 403, and the server logs a stack trace per request.
func TestRound2OriginWithEscapedPercentOverTheWire(t *testing.T) {
	srv := httptest.NewServer(NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{}))
	defer srv.Close()
	srv.Config.ErrorLog = nil
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(round2Ping))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://a%25b")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Origin http://a%%25b: no HTTP answer, the handler panicked: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// Round 2: in a batch, an element that fails to decode is answered with a
// null id even when its id was decoded (json.Unmarshal keeps going after a
// type error), while the same message on its own is answered with its id.
// JSON-RPC uses null only when the id cannot be detected, and a 2025-03-26
// client matching replies by id never sees an answer to request 7.
func TestRound2BatchElementWithAKnownIDKeepsItInTheError(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	bad := `{"jsonrpc":"2.0","id":7,"method":5}`

	single := post(t, h, bad, nil)
	require.Equal(t, http.StatusBadRequest, single.Code)
	assert.Contains(t, single.Body.String(), `"id":7`, "on its own")

	batch := post(t, h, "["+bad+","+round2Ping+"]", nil)
	require.Equal(t, http.StatusOK, batch.Code)
	var replies []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(batch.Body.Bytes(), &replies), batch.Body.String())
	var ids []string
	for _, r := range replies {
		ids = append(ids, string(r["id"]))
	}
	assert.ElementsMatch(t, []string{"7", "1"}, ids, "batch answer %s", batch.Body.String())
}

// Round 2: NormalizeOrigin says it returns an origin "as browsers send
// it", but it only lower-cases an IPv6 literal. Browsers serialise the
// compressed form, so an allowed origin written in another valid IPv6
// spelling never matches and that page is refused.
func TestRound2NormalizeOriginCanonicalisesIPv6(t *testing.T) {
	for given, browser := range map[string]string{
		"http://[2001:db8:0:0:0:0:0:1]:8080": "http://[2001:db8::1]:8080",
		"https://[2001:DB8::0:1]":            "https://[2001:db8::1]",
	} {
		got, err := NormalizeOrigin(given)
		require.NoError(t, err, given)
		assert.Equal(t, browser, got, given)

		h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{given}})
		rec := post(t, h, round2Ping, map[string]string{"Origin": browser})
		assert.Equal(t, http.StatusOK, rec.Code, "allowed %s, browser sends %s", given, browser)
	}
}

// Round 2: before the fix an allowed origin was compared as given, so
// --allow-origin admitted non-http browser origins such as an extension's
// or a VS Code webview's. NormalizeOrigin now refuses every scheme but http
// and https, HTTPHandler drops such entries silently and the CLI refuses
// them at startup, so these pages can no longer be allowed at all.
func TestRound2NonHTTPBrowserOriginCanStillBeAllowed(t *testing.T) {
	for _, origin := range []string{"chrome-extension://abcdefghijklmnopabcdefghijklmnop", "vscode-webview://1a2b3c"} {
		h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{origin}})
		rec := post(t, h, round2Ping, map[string]string{"Origin": origin})
		assert.Equal(t, http.StatusOK, rec.Code, "explicitly allowed %s: %d %q", origin, rec.Code, rec.Body.String())
	}
}

// Pre-existing (stdio, unchanged by 68a8d6a and 276cdb6): the server
// negotiates 2025-03-26, whose stdio transport allows batches, and the HTTP
// handler now answers batches and undecodable messages with a known id. The
// stdio reader drops any line that does not decode into one message, so a
// batch or a request with a mistyped field is never answered and the client
// waits for ever.
func TestRound2StdioAnswersWhatHTTPAnswers(t *testing.T) {
	for _, line := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":1,"method":5}`,
	} {
		out := &bytes.Buffer{}
		s := NewLazy(noEngine, strings.NewReader(line+"\n"), out)
		require.NoError(t, s.Run(context.Background()))
		assert.Contains(t, out.String(), `"id":1`, "stdio line %s got no answer", line)
	}
}

// Probes that hold on 276cdb6 (kept as regression checks).
func TestRound2MessageHandlingThatHolds(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: strings.Repeat("k", 32), AllowedOrigins: []string{"https://ok.example"}})
	auth := map[string]string{"Authorization": "Bearer " + strings.Repeat("k", 32)}

	// Responses with a null result or error are still responses.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"x"}}`,
		`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
		`[{"jsonrpc":"2.0","method":"no/such"},{"jsonrpc":"2.0","id":2,"result":{}}]`,
	} {
		rec := post(t, h, body, auth)
		assert.Equal(t, http.StatusAccepted, rec.Code, "%s: %q", body, rec.Body.String())
		assert.Empty(t, rec.Body.String(), body)
	}
	for body, want := range map[string]int{
		`[1]`:     200,
		`[null]`:  200,
		`[[]]`:    200,
		`1`:       400,
		`true`:    400,
		`{"a":1}`: 400,
		`[`:       400,
	} {
		assert.Equal(t, want, post(t, h, body, auth).Code, body)
	}

	// Preflight: allowed origin answered before the token check; others not.
	for origin, want := range map[string]int{"https://ok.example": 204, "http://localhost:5173": 204, "https://evil.example": 403, "null": 403} {
		req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, origin)
		if want == 204 {
			assert.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"))
		} else {
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), origin)
		}
	}
	// OPTIONS without Origin is not a preflight: token first.
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	// A 401 to an allowed page is readable, with WWW-Authenticate exposed.
	rec = post(t, h, round2Ping, map[string]string{"Origin": "https://ok.example"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "https://ok.example", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "WWW-Authenticate")

	for header, want := range map[string]int{
		"Bearer " + strings.Repeat("k", 32):       200,
		"bearer " + strings.Repeat("k", 32):       200,
		"Bearer   " + strings.Repeat("k", 32):     200,
		"Bearer\t" + strings.Repeat("k", 32):      401,
		"Bearer " + strings.Repeat("k", 32) + "x": 401,
		"Bearer":                           401,
		"Bearer ":                          401,
		"Basic " + strings.Repeat("k", 32): 401,
	} {
		assert.Equal(t, want, post(t, h, round2Ping, map[string]string{"Authorization": header}).Code, "%q", header)
	}

	for in, want := range map[string]string{
		"HTTPS://OK.Example:443/": "https://ok.example",
		"http://[::1]":            "http://[::1]",
		"http://[::1]:80":         "http://[::1]",
		" https://a.example ":     "https://a.example",
		"http://a.example:8080":   "http://a.example:8080",
	} {
		got, err := NormalizeOrigin(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"*", "https://*.example", "null", "", "https://a.example/x", "https://u@a.example", "ftp://a.example", "https://a.example?q"} {
		_, err := NormalizeOrigin(in)
		assert.Error(t, err, fmt.Sprintf("%q", in))
	}
}
