package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/mcpserver"
	"github.com/argon-lab/argon/v2/pkg/walcli"
)

// Round 3 finding (nit): checkHTTPListen refuses tokens "no client can send"
// (spaces, control characters, more than the header limit) but accepts a
// non-ASCII ARGON_MCP_TOKEN, compared as UTF-8 bytes. Browsers' fetch and
// Node's http (Claude Code, Cursor and most MCP clients) send a header value
// as one byte per character and refuse characters above U+00FF, so the bytes
// they send for "é" are 0xE9, not the 0xC3 0xA9 the server compares against.
// The server starts and every such client is refused with 401, with nothing
// saying why.
func TestRound3CheckHTTPListenRefusesANonASCIIToken(t *testing.T) {
	token := strings.Repeat("é", 16) + strings.Repeat("a", 16) // 48 bytes, 32 characters
	listenErr := checkHTTPListen("0.0.0.0:8765", token)
	if listenErr != nil {
		return
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := mcpserver.NewLazy(func(context.Context) (*walcli.Services, error) {
		return nil, errors.New("no engine")
	}, nil, nil)
	go func() { done <- server.ServeHTTP(ctx, listener, mcpserver.HTTPConfig{Token: token}) }()
	defer func() { cancel(); <-done }()

	// The header as a browser or Node client puts it on the wire: one byte
	// per character of the string the user pasted.
	latin1 := make([]byte, 0, len(token))
	for _, r := range token {
		latin1 = append(latin1, byte(r))
	}
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		latin1, len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkHTTPListen accepted the non-ASCII token %q, yet a browser or Node client sending it gets %d", token, resp.StatusCode)
	}
}
