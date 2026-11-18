package cmd

import "github.com/spf13/cobra"

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check database readiness and capture health",
	RunE:  func(cmd *cobra.Command, args []string) error { return runDeploymentCommand(cmd, false) },
}

func init() { rootCmd.AddCommand(statusCmd) }
