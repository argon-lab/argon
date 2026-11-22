package mcpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Round 3 (nit): 7471d41 replaced "http or https only" with a list of
// schemes it calls the ones browsers send, so that an extension or VS Code
// webview can be allowed. The list leaves out the webview origins of the
// common desktop and mobile app shells: Tauri serves its pages as
// tauri://localhost (macOS, Linux, iOS) and Capacitor as
// capacitor://localhost (iOS), and their webviews send that as Origin on a
// cross-origin fetch. Such an app cannot be allowed at all: `argon mcp
// --http ... --allow-origin tauri://localhost` refuses to start, saying
// browsers do not send that scheme, and a library caller's HTTPConfig entry
// is dropped silently, so every request from the app gets 403.
func TestRound3AppShellOriginsCanBeAllowed(t *testing.T) {
	for _, origin := range []string{"tauri://localhost", "capacitor://localhost"} {
		_, err := NormalizeOrigin(origin)
		assert.NoError(t, err, "--allow-origin %s is refused at startup", origin)

		h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{AllowedOrigins: []string{origin}})
		rec := post(t, h, round2Ping, map[string]string{"Origin": origin})
		assert.Equal(t, http.StatusOK, rec.Code, "explicitly allowed %s: %d %q", origin, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
