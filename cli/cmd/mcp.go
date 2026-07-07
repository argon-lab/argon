package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/argon-lab/argon/pkg/mcpserver"
	"github.com/argon-lab/argon/pkg/walcli"
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

  claude mcp add argon -- argon mcp`,
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		return mcpserver.New(services, os.Stdin, os.Stdout).Run(context.Background())
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
