package cmd

import (
	"strings"
	"testing"
)

// Review (nit): an address with no port is refused with "needs
// ARGON_MCP_TOKEN", which is not the problem: with a token set the user
// then gets net.Listen's "missing port". Uppercase LOCALHOST, which
// resolves to loopback, is also treated as non-loopback.
func TestReviewCheckHTTPListenNamesTheRealProblem(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "localhost"} {
		err := checkHTTPListen(addr, "")
		if err != nil && strings.Contains(err.Error(), "ARGON_MCP_TOKEN") {
			t.Errorf("%s: blamed the token for a missing port: %v", addr, err)
		}
	}
	if err := checkHTTPListen("LOCALHOST:8765", ""); err != nil {
		t.Errorf("LOCALHOST:8765: %v", err)
	}
}
