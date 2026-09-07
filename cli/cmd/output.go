package cmd

import (
	"encoding/json"
	"github.com/spf13/cobra"
)

func jsonOutput(cmd *cobra.Command) bool {
	format, _ := cmd.Flags().GetString("output")
	return format == "json"
}

// One JSON value per successful command; diagnostics stay on stderr.
func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func init() {
	for _, command := range []*cobra.Command{
		projectsCreateCmd, projectsListCmd, branchesCreateCmd, branchesListCmd, branchesDeleteCmd,
		sandboxCreateCmd, sandboxListCmd, sandboxDiscardCmd, sandboxKeepCmd, sandboxSweepCmd,
		pinCreateCmd, pinListCmd, pinDeleteCmd, pinBranchCmd, pinSandboxCmd,
		diffCmd, mergePreviewCmd, mergeApplyCmd, mergeListCmd, undoCmd,
		checkoutCmd, connectCmd, releaseCmd, statusCmd, doctorCmd,
		timeTravelInfoCmd, timeTravelQueryCmd, snapshotCreateCmd, snapshotListCmd,
	} {
		command.Flags().StringP("output", "o", "table", "Output format: table or json")
	}
}
