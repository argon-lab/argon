package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Probes that hold on 7471d41 (kept as regression checks): stdio and HTTP
// answer every input class alike since both go through decode, and stdio
// writes exactly one JSON value per answered line.
func TestRound3DecodeIsTheSameOverBothTransports(t *testing.T) {
	h := NewLazy(noEngine, nil, nil).HTTPHandler(HTTPConfig{})
	inputs := []string{
		round2Ping,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{"a" : [1, 2]},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"a b","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`[[{"jsonrpc":"2.0","id":1,"method":"ping"}]]`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},[{"jsonrpc":"2.0","id":2,"method":"ping"}],{"method":5,"id":3,"jsonrpc":"2.0"}]`,
		`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":1,"result":{}}]`,
		`[]`, `[1,2]`, `null`, `"x"`, `5`, `{"a":1}`, `{not json`, `{"jsonrpc":"2.0","id":1,"method":"ping"} {"id":2}`,
	}
	for _, in := range inputs {
		rec := post(t, h, in, nil)
		out := &bytes.Buffer{}
		require.NoError(t, NewLazy(noEngine, strings.NewReader(in+"\n"), out).Run(context.Background()))
		if rec.Code == http.StatusAccepted {
			assert.Empty(t, out.String(), "stdio answered what HTTP accepted silently: %s", in)
			continue
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		require.Len(t, lines, 1, "stdio wrote %d lines for %s: %q", len(lines), in, out.String())
		assert.True(t, json.Valid([]byte(lines[0])), in)
		assert.JSONEq(t, rec.Body.String(), lines[0], "stdio and HTTP differ for %s", in)
	}
	// A batch element whose id follows its bad field keeps the id.
	rec := post(t, h, `[{"method":5,"id":3,"jsonrpc":"2.0"}]`, nil)
	assert.Contains(t, rec.Body.String(), `"id":3`)
}

// Probes that hold on 7471d41: origins a foreign page could present are
// refused even after normalisation, and non-http schemes never get the
// loopback pass.
func TestRound3OriginBypassesAreRefused(t *testing.T) {
	allowed := map[string]bool{}
	for _, o := range []string{"https://console.example.com", "chrome-extension://abcdefghijklmnopabcdefghijklmnop"} {
		n, err := NormalizeOrigin(o)
		require.NoError(t, err)
		allowed[n] = true
	}
	for origin, want := range map[string]bool{
		"https://console.example.com":                         true,
		"HTTPS://CONSOLE.EXAMPLE.COM:443":                     true,
		"http://console.example.com":                          false,
		"https://console.example.com.":                        false,
		"https://console.example.com.evil.com":                false,
		"https://evil.com#@console.example.com":               false,
		"https://console.example.com@evil.com":                false,
		"https://evil.com/console.example.com":                false,
		"http://127.0.0.1.evil.com":                           false,
		"http://localhost.evil.com":                           false,
		"http://0.0.0.0:3000":                                 false,
		"chrome-extension://localhost":                        false,
		"vscode-webview://127.0.0.1":                          false,
		"chrome-extension://ABCDEFGHIJKLMNOPABCDEFGHIJKLMNOP": true,
		"chrome-extension://bbcdefghijklmnopabcdefghijklmnop": false,
		"http://[::ffff:7f00:1]:3000":                         true, // loopback
		"http://[::1]:3000":                                   true,
		"http://%6cocalhost":                                  false,
		"http://localhost%00.evil.com":                        false,
		"null":                                                false,
	} {
		assert.Equal(t, want, originAllowed(origin, allowed), origin)
	}
}
