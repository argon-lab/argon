package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

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
pin list).`,
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
		return mcpserver.NewLazy(connect, os.Stdin, os.Stdout, opts...).Run(context.Background())
	},
}

func init() {
	mcpCmd.Flags().Bool("read-only", false, "Offer only tools that inspect state")
	mcpCmd.Flags().StringSlice("disable-tools", nil, "Tools to withhold, comma-separated (e.g. argon_merge_apply)")
	rootCmd.AddCommand(mcpCmd)
}
