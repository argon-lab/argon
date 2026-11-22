package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Round 2 finding (nit): NormalizeOrigin, which the CLI now uses to validate
// --allow-origin, accepts an internationalised host in its Unicode form.
// Browsers serialise the Origin header with the host in punycode, so the
// configured origin can never match: the CLI starts without complaint and
// the console it was meant for gets 403 on every request.
func TestRound2AnIDNAllowedOriginEitherMatchesOrIsRefused(t *testing.T) {
	const configured = "https://bücher.example"
	const sentByBrowser = "https://xn--bcher-kva.example" // what a page on that host sends

	if _, err := NormalizeOrigin(configured); err != nil {
		return // refused at startup: the user is told
	}
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{configured}})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Origin", sentByBrowser)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("--allow-origin %s was accepted, yet a page on that host (Origin: %s) gets %d %q",
			configured, sentByBrowser, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
