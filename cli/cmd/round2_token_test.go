package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/mcpserver"
	"github.com/argon-lab/argon/v2/pkg/walcli"
)

// Round 2 finding (nit): the fix gave ServeHTTP MaxHeaderBytes of 64 KiB and
// made checkHTTPListen refuse "tokens no client can send", but it accepts a
// token too long to fit in that header limit. The server starts and every
// request, carrying exactly that token, is refused with 431.
func TestRound2CheckHTTPListenRefusesATokenLongerThanTheHeaderLimit(t *testing.T) {
	token := strings.Repeat("ab", 40<<10) // 80 KiB of hex-like characters
	listenErr := checkHTTPListen("127.0.0.1:0", token)
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

	req, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkHTTPListen accepted a %d-byte token, yet a client sending exactly that token gets %d", len(token), resp.StatusCode)
	}
}
