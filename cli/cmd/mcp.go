package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/argon-lab/argon/v2/pkg/mcpserver"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve Argon to AI agents over the Model Context Protocol",
	Long: `Runs an MCP server on stdin/stdout exposing the agent workflow as
tools: fork a TTL sandbox and get a connection string, list branches,
diff and merge, undo, snapshot. The server supervises change-stream
capture for every sandbox it hands out, so agent writes become
versioned history automatically.

The server starts without a database. Listing its tools needs no
connection. It reaches for MONGODB_URI in the background from the start,
so expired sandboxes are swept even when no tool is called. If
MongoDB is not reachable when a tool is called, the call fails with an error that says
so, the server keeps running, and the next call tries again.

Register it with an MCP client, e.g. Claude Code:

  claude mcp add argon -- argon mcp

By default an agent can do everything the tools allow, including applying
a merge plan it prepared itself. To keep that step with a person, withhold
the tool and apply reviewed plans with "argon merge apply <plan-id>":

  argon mcp --disable-tools argon_merge_apply

--read-only offers only the tools that inspect state (branch list, diff,
pin list).

--http serves the same tools over MCP's Streamable HTTP transport at
http://<addr>/mcp instead of stdin/stdout, for clients on another machine
or a hosted control plane:

  ARGON_MCP_TOKEN=<token> argon mcp --http 127.0.0.1:8765
  claude mcp add --transport http argon http://127.0.0.1:8765/mcp \
    --header "Authorization: Bearer <token>"

When ARGON_MCP_TOKEN is set every request must carry it as a bearer
token. A loopback address may run without one; any other address
requires it. Browser requests are accepted only from loopback origins and
those named with --allow-origin. The server keeps no sessions and offers no
server-initiated stream: each POST is answered with one JSON body.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		readOnly, _ := cmd.Flags().GetBool("read-only")
		disabled, _ := cmd.Flags().GetStringSlice("disable-tools")
		if unknown := mcpserver.UnknownTools(disabled); len(unknown) > 0 {
			return fmt.Errorf("--disable-tools names unknown tool(s): %s", strings.Join(unknown, ", "))
		}
		opts := []mcpserver.Option{mcpserver.DisableTools(disabled...), mcpserver.ConnectInBackground()}
		if readOnly {
			opts = append(opts, mcpserver.ReadOnly())
		}
		// A client may start the server only to ask what it offers, on a
		// machine with no database. Connect when a tool is called instead.
		connect := func(ctx context.Context) (*walcli.Services, error) {
			return connectCommandServices(ctx, cmd)
		}
		addr, _ := cmd.Flags().GetString("http")
		if addr == "" {
			return mcpserver.NewLazy(connect, os.Stdin, os.Stdout, opts...).Run(context.Background())
		}
		origins, _ := cmd.Flags().GetStringSlice("allow-origin")
		for _, origin := range origins {
			if _, err := mcpserver.NormalizeOrigin(origin); err != nil {
				return fmt.Errorf("--allow-origin: %w", err)
			}
		}
		cfg := mcpserver.HTTPConfig{Token: os.Getenv("ARGON_MCP_TOKEN"), AllowedOrigins: origins}
		if err := checkHTTPListen(addr, cfg.Token); err != nil {
			return err
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// The first signal stops the server gracefully: calls in flight
		// finish and capture is stopped. A second one ends the process.
		go func() { <-ctx.Done(); stop() }()
		auth := "no token"
		if cfg.Token != "" {
			auth = "bearer token from ARGON_MCP_TOKEN"
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "argon mcp: serving MCP over HTTP at http://%s/mcp (%s)\n", listener.Addr(), auth)
		return mcpserver.NewLazy(connect, nil, nil, opts...).ServeHTTP(ctx, listener, cfg)
	},
}

// minHTTPTokenLength keeps a guessable token off a network listener.
const minHTTPTokenLength = 32

// maxHTTPTokenLength keeps the Authorization header well inside the
// transport's header limit.
const maxHTTPTokenLength = 4096

func checkHTTPListen(addr, token string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("--http needs host:port, e.g. 127.0.0.1:8765: %v", err)
	}
	// A client cannot send a header value with whitespace, control
	// characters or (browsers and Node) non-ASCII, so such a token would lock
	// clients out.
	for _, r := range token {
		if r <= ' ' || r >= 0x7f {
			return errors.New("ARGON_MCP_TOKEN must be printable ASCII without spaces (for example: openssl rand -hex 32)")
		}
	}
	if len(token) > maxHTTPTokenLength {
		return fmt.Errorf("ARGON_MCP_TOKEN must be at most %d characters", maxHTTPTokenLength)
	}
	if token != "" && len(token) < minHTTPTokenLength {
		return fmt.Errorf("ARGON_MCP_TOKEN must be at least %d characters (for example: openssl rand -hex 32)", minHTTPTokenLength)
	}
	if token == "" && !mcpserver.IsLoopbackAddress(addr) {
		return errors.New("--http on a non-loopback address needs ARGON_MCP_TOKEN: every tool can read and change your databases, so the server will not listen there without a bearer token")
	}
	return nil
}

func init() {
	mcpCmd.Flags().String("http", "", "Serve MCP over Streamable HTTP on this address (e.g. 127.0.0.1:8765) instead of stdio")
	mcpCmd.Flags().StringSlice("allow-origin", nil, "Browser origins allowed to call the HTTP server, besides loopback ones")
	mcpCmd.Flags().Bool("read-only", false, "Offer only tools that inspect state")
	mcpCmd.Flags().StringSlice("disable-tools", nil, "Tools to withhold, comma-separated (e.g. argon_merge_apply)")
	rootCmd.AddCommand(mcpCmd)
}
