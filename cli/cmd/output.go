package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func jsonOutput(cmd *cobra.Command) bool {
	format, _ := cmd.Flags().GetString("output")
	return format == "json"
}

// One JSON value per successful command; diagnostics stay on stderr.
func writeJSON(cmd *cobra.Command, value any) error {
	return encodeJSON(cmd.OutOrStdout(), value)
}

func encodeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func init() {
	for _, command := range []*cobra.Command{
		projectsCreateCmd, projectsListCmd, branchesCreateCmd, branchesListCmd, branchesDeleteCmd,
		sandboxCreateCmd, sandboxListCmd, sandboxDiscardCmd, sandboxKeepCmd, sandboxSweepCmd,
		pinCreateCmd, pinListCmd, pinDeleteCmd, pinBranchCmd, pinSandboxCmd,
		diffCmd, mergePreviewCmd, mergeCheckCmd, mergeApplyCmd, mergeListCmd, undoCmd,
		checkoutCmd, connectCmd, releaseCmd, statusCmd, doctorCmd,
		timeTravelInfoCmd, timeTravelQueryCmd, snapshotCreateCmd, snapshotListCmd,
		gcCmd, restorePreviewCmd, restoreResetCmd, restoreBranchCmd,
	} {
		command.Flags().StringP("output", "o", "table", "Output format: table or json")
	}
}

// printIndexNotes lists the indexes a checkout built differently from their
// recorded definition or could not build.
func printIndexNotes(cmd *cobra.Command, notes []string) {
	for _, note := range notes {
		fmt.Fprintf(cmd.OutOrStdout(), "Note: %s\n", note)
	}
}
