package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func post(t *testing.T, h http.Handler, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func noEngine(context.Context) (*walcli.Services, error) {
	return nil, errors.New("introspection must not connect")
}

func TestHTTPAnswersRequestsAsJSON(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var reply map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	assert.Equal(t, "2025-06-18", reply["result"].(map[string]interface{})["protocolVersion"])

	rec = post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "argon_sandbox_create")

	// A notification (and a client's response) is accepted with no body.
	rec = post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, rec.Body.String())

	// 2025-03-26 clients may batch.
	rec = post(t, h, `[{"jsonrpc":"2.0","id":3,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var batch []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &batch), rec.Body.String())
	assert.Len(t, batch, 1)
}

func TestHTTPToolCallReportsAnUnreachableDatabase(t *testing.T) {
	connect := func(context.Context) (*walcli.Services, error) {
		return nil, walcli.ErrUnreachable
	}
	h := NewLazy(connect, nil, nil).HTTPHandler(HTTPConfig{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"argon_branch_list","arguments":{"project":"shop"}}}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"isError":true`)
	assert.Contains(t, rec.Body.String(), "MongoDB is not reachable")
}

func TestHTTPRequiresTheBearerToken(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{Token: "s3cret-token"})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	for _, auth := range []string{"", "Bearer wrong", "Basic czNjcmV0LXRva2Vu", "Bearer s3cret-token-and-more", "bearer"} {
		rec := post(t, h, ping, map[string]string{"Authorization": auth})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "Authorization %q", auth)
		assert.Equal(t, `Bearer realm="argon"`, rec.Header().Get("WWW-Authenticate"))
		assert.NotContains(t, rec.Body.String(), "s3cret")
	}
	rec := post(t, h, ping, map[string]string{"Authorization": "Bearer s3cret-token"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestHTTPRefusesForeignOrigins(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{"https://cloud.argonlabs.tech"}})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for origin, want := range map[string]int{
		"":                             http.StatusOK, // not a browser
		"http://localhost:5173":        http.StatusOK,
		"http://127.0.0.1:3000":        http.StatusOK,
		"https://cloud.argonlabs.tech": http.StatusOK,
		"https://evil.example":         http.StatusForbidden,
		"http://localhost.evil.com":    http.StatusForbidden,
		"null":                         http.StatusForbidden,
	} {
		rec := post(t, h, ping, map[string]string{"Origin": origin})
		assert.Equal(t, want, rec.Code, "Origin %q", origin)
	}
}

func TestHTTPRejectsWhatItDoesNotServe(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})

	// No server-initiated stream is offered, so GET is not allowed.
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, get.Code)
	assert.Equal(t, "POST", get.Header().Get("Allow"))

	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = post(t, h, `not json`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "-32700")

	big := httptest.NewRequest(http.MethodPost, "/mcp", io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping","params":"`), bytes.NewReader(bytes.Repeat([]byte("a"), maxHTTPBody)), strings.NewReader(`"}`)))
	bigRec := httptest.NewRecorder()
	h.ServeHTTP(bigRec, big)
	assert.Equal(t, http.StatusRequestEntityTooLarge, bigRec.Code)
}

func TestListenAddressNeedsATokenOffLoopback(t *testing.T) {
	for addr, loopback := range map[string]bool{
		"127.0.0.1:8765": true,
		"localhost:8765": true,
		"[::1]:8765":     true,
		":8765":          false,
		"0.0.0.0:8765":   false,
		"10.0.0.5:8765":  false,
	} {
		assert.Equal(t, loopback, IsLoopbackAddress(addr), addr)
	}
}
