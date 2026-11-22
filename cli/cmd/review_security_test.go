package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/mcpserver"
	"github.com/argon-lab/argon/v2/pkg/walcli"
)

// Review finding: checkHTTPListen accepts an ARGON_MCP_TOKEN with trailing
// whitespace (an env file with CRLF line ends, a quoted value with a stray
// space) or made only of whitespace (which also passes the 32-character
// minimum). net/http trims header values, so no client can ever present such
// a token: the server starts and refuses every request with 401, with nothing
// saying why.
func TestReviewCheckHTTPListenRefusesATokenNoClientCanSend(t *testing.T) {
	good := strings.Repeat("ab", 16)
	for name, token := range map[string]string{
		"trailing CR":     good + "\r",
		"trailing space":  good + " ",
		"only whitespace": strings.Repeat(" ", 32),
	} {
		t.Run(name, func(t *testing.T) {
			listenErr := checkHTTPListen("127.0.0.1:0", token)

			h := mcpserver.NewLazy(func(context.Context) (*walcli.Services, error) {
				return nil, errors.New("no engine")
			}, nil, nil).HTTPHandler(mcpserver.HTTPConfig{Token: token})
			server := httptest.NewServer(h)
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			req.Header.Set("Authorization", "Bearer "+token)
			status := 0
			if resp, err := http.DefaultClient.Do(req); err == nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			if listenErr == nil && status != http.StatusOK {
				t.Fatalf("checkHTTPListen accepted token %q, yet a client sending exactly that token gets status %d (0 = client refused to send it)", token, status)
			}
		})
	}
}
