package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/argon-lab/argon/v2/pkg/mcpserver"
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
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		opts := []mcpserver.Option{mcpserver.DisableTools(disabled...)}
		if readOnly {
			opts = append(opts, mcpserver.ReadOnly())
		}
		return mcpserver.New(services, os.Stdin, os.Stdout, opts...).Run(context.Background())
	},
}

func init() {
	mcpCmd.Flags().Bool("read-only", false, "Offer only tools that inspect state")
	mcpCmd.Flags().StringSlice("disable-tools", nil, "Tools to withhold, comma-separated (e.g. argon_merge_apply)")
	rootCmd.AddCommand(mcpCmd)
}
