package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var undoCmd = &cobra.Command{
	Use:   "undo",
	Short: "Revert a range of a branch's history",
	Long: `Undo restores every document touched in an LSN range to its state
just before the range began, using the pre-images recorded in the WAL.
History stays append-only: the undo itself is written as new history, so
it is audited and can be undone in turn.

With --actor, only that actor's writes are reverted; documents another
actor modified afterwards are reported as conflicts and skipped.

Use "argon time-travel info" to find LSNs, and --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		fromLSN, _ := cmd.Flags().GetInt64("from-lsn")
		toLSN, _ := cmd.Flags().GetInt64("to-lsn")
		actor, _ := cmd.Flags().GetString("actor")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if fromLSN <= 0 {
			return fmt.Errorf("--from-lsn is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}

		plan, err := services.BuildUndoPlan(branchID, fromLSN, toLSN, actor)
		if err != nil {
			return fmt.Errorf("failed to plan undo: %w", err)
		}

		fmt.Printf("Undo [%d, %d]", plan.FromLSN, plan.ToLSN)
		if plan.Actor != "" {
			fmt.Printf(" for actor %q", plan.Actor)
		}
		fmt.Printf(": %d document(s) to revert\n", len(plan.Compensations))
		for _, c := range plan.Compensations {
			action := "restore"
			if c.Restore == nil {
				action = "delete "
			}
			fmt.Printf("  %s %s/%s\n", action, c.Collection, c.DocumentID)
		}
		for _, c := range plan.Conflicts {
			fmt.Printf("  CONFLICT %s/%s: modified by %q at LSN %d — skipped\n",
				c.Collection, c.DocumentID, c.OtherActor, c.AtLSN)
		}
		for _, u := range plan.Unrecoverable {
			fmt.Printf("  UNRECOVERABLE %s: no pre-image available — skipped\n", u)
		}

		if dryRun {
			fmt.Println("Dry run: nothing applied.")
			return nil
		}

		restored, deleted, err := services.ApplyUndoPlan(context.Background(), branchID, plan)
		if err != nil {
			return fmt.Errorf("undo failed: %w", err)
		}
		fmt.Printf("Done: %d restored, %d deleted.\n", restored, deleted)
		return nil
	},
}

func init() {
	undoCmd.Flags().StringP("project", "p", "", "Project name (required)")
	undoCmd.Flags().StringP("branch", "b", "", "Branch name (default: main)")
	undoCmd.Flags().Int64("from-lsn", 0, "Start of the range to revert (required)")
	undoCmd.Flags().Int64("to-lsn", 0, "End of the range (default: branch head)")
	undoCmd.Flags().String("actor", "", "Only revert writes by this actor")
	undoCmd.Flags().Bool("dry-run", false, "Preview without applying")
	rootCmd.AddCommand(undoCmd)
}
