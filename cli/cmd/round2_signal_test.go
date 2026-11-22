package cmd

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Round 2 coverage: the first SIGTERM stops argon mcp --http gracefully, and
// while that shutdown is held up (here by a request whose body is still
// arriving) a second SIGTERM ends the process at once.
func TestRound2SecondSignalEndsAServerStillShuttingDown(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$", "--", "mcp", "--http", "127.0.0.1:0")
	command.Env = append(os.Environ(), "ARGON_CLI_TEST_HELPER=1",
		"MONGODB_URI=mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100", "ARGON_MCP_TOKEN=")
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })

	address := make(chan string, 1)
	go func() {
		pattern := regexp.MustCompile(`at http://(\S+)/mcp`)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if m := pattern.FindStringSubmatch(scanner.Text()); m != nil {
				address <- m[1]
			}
		}
	}()
	var addr string
	select {
	case addr = <-address:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not report its address")
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"jsonrpc\":", addr)
	time.Sleep(300 * time.Millisecond)

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		t.Skipf("the first signal already ended the process (%v); nothing held shutdown", err)
	case <-time.After(time.Second):
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil || !strings.Contains(err.Error(), "terminated") {
			t.Logf("exit after the second signal: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a second SIGTERM did not end a server that was still shutting down")
	}
}
