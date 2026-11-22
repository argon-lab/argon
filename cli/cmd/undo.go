package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

type undoPlan = walcli.UndoPlan

// requireBranchInProject refuses a merge plan whose target is outside the
// project the command was given.
func requireBranchInProject(services *walcli.Services, projectName, branchID string) error {
	project, err := services.Projects.GetProjectByName(projectName)
	if err != nil {
		return err
	}
	branch, err := services.Branches.GetBranchByID(branchID)
	if err != nil {
		return err
	}
	if branch.ProjectID != project.ID {
		return fmt.Errorf("merge plan's target branch %s is not in project %s", branch.Name, projectName)
	}
	return nil
}

var undoCmd = &cobra.Command{
	Use:   "undo",
	Short: "Revert a range of a branch's history",
	Long: `Undo restores every document touched in an LSN range to its state
just before the range began, using the pre-images recorded in the WAL.
History stays append-only: the undo itself is written as new history, so
it is audited and can be undone in turn.

With --actor, only that actor's writes are reverted; documents another
actor modified afterwards are reported as conflicts and skipped.

With --merge <plan-id>, exactly the changes that merge plan applied are
reverted, on the branch it was merged into, including a checked-out branch
whose merged writes capture recorded. Later writes to the same documents are
reported as conflicts and skipped. The undo is refused while capture has not
recorded the whole merge.

Use "argon time-travel info" to find LSNs, and --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		fromLSN, _ := cmd.Flags().GetInt64("from-lsn")
		toLSN, _ := cmd.Flags().GetInt64("to-lsn")
		actor, _ := cmd.Flags().GetString("actor")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		mergePlan, _ := cmd.Flags().GetString("merge")
		if mergePlan != "" {
			if fromLSN != 0 || toLSN != 0 || actor != "" || branchName != "" {
				return fmt.Errorf("--merge selects the merge's own changes on the branch it was merged into; it cannot be combined with --branch, --from-lsn, --to-lsn or --actor")
			}
		} else {
			if projectName == "" {
				return fmt.Errorf("--project is required")
			}
			if fromLSN <= 0 {
				return fmt.Errorf("--from-lsn is required")
			}
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		var branchID string
		var plan *undoPlan
		if mergePlan != "" {
			branchID, plan, err = services.BuildMergeUndoPlan(context.Background(), mergePlan)
			if err != nil {
				return fmt.Errorf("failed to plan undo: %w", err)
			}
			if projectName != "" {
				if err := requireBranchInProject(services, projectName, branchID); err != nil {
					return err
				}
			}
		} else {
			branchID, err = resolveBranch(services, projectName, branchName)
			if err != nil {
				return err
			}
			plan, err = services.BuildUndoPlan(branchID, fromLSN, toLSN, actor)
			if err != nil {
				return fmt.Errorf("failed to plan undo: %w", err)
			}
		}

		if !jsonOutput(cmd) {
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

		}

		if dryRun {
			if jsonOutput(cmd) {
				return writeJSON(cmd, map[string]any{"dry_run": true, "plan": plan})
			}
			fmt.Println("Dry run: nothing applied.")
			return nil
		}

		restored, deleted, err := services.ApplyUndoPlan(context.Background(), branchID, plan)
		if err != nil {
			return fmt.Errorf("undo failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"dry_run": false, "plan": plan, "restored": restored, "deleted": deleted})
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
	undoCmd.Flags().String("merge", "", "Revert exactly the changes of this applied merge plan")
	rootCmd.AddCommand(undoCmd)
}
