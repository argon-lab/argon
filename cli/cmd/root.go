package cmd

import (
	"errors"
	"fmt"

	"github.com/argon-lab/argon/pkg/version"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "argon",
	Short: "Branch, review and recover MongoDB data",
	Long: `Argon versions MongoDB data with branches, merge plans, pins and undo.

Start with "argon doctor", then "argon console" for a managed local API
and web console. Use MONGODB_URI to select the MongoDB replica set.
The CLI connects directly to MongoDB; API bearer tokens configure the
HTTP server through ARGON_API_TOKEN, not individual CLI commands.`,
	Version:      version.String(),
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flag := cmd.Flags().Lookup("output"); flag != nil {
			format, _ := cmd.Flags().GetString("output")
			if format != "table" && format != "json" {
				return fmt.Errorf("unsupported output %q; use table or json", format)
			}
		}
		return nil
	},
}

func Execute() error {
	err := rootCmd.Execute()
	cleanupErr := closeCommandServices()
	if cleanupErr != nil {
		fmt.Fprintf(rootCmd.ErrOrStderr(), "Error: command cleanup: %v\n", cleanupErr)
	}
	return errors.Join(err, cleanupErr)
}
