package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
)

var collectionsCmd = &cobra.Command{Use: "collections", Short: "Prepare native collections for exact write capture"}
var collectionsPrepareCmd = &cobra.Command{
	Use: "prepare <name>", Short: "Create or enable exact images before the first application write", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		branch, _ := cmd.Flags().GetString("branch")
		services, err := newCommandServices(cmd)
		if err != nil {
			return err
		}
		id, err := resolveBranch(services, project, branch)
		if err != nil {
			return err
		}
		if err := services.PrepareCollection(cmd.Context(), id, args[0]); err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": id, "collection": args[0], "exact_images": true})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Collection %s is ready for exact images; keep capture running while writing.\n", args[0])
		return nil
	},
}

func init() {
	collectionsPrepareCmd.Flags().StringP("project", "p", "", "Project name (required)")
	collectionsPrepareCmd.Flags().StringP("branch", "b", "main", "Branch name")
	collectionsPrepareCmd.Flags().StringP("output", "o", "table", "Output format: table or json")
	_ = collectionsPrepareCmd.MarkFlagRequired("project")
	collectionsCmd.AddCommand(collectionsPrepareCmd)
	rootCmd.AddCommand(collectionsCmd)
}
