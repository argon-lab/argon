package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Reclaim WAL entries covered by snapshots and outside retention",
	Long: `Deletes WAL entries that no reader can need anymore: entries at or
below the newest snapshot that covers them, older than the retention
window, and not pinned by a live child branch's fork point. History
without snapshot coverage is never deleted, no matter how old.

Reclaiming entries ends time-travel, audit and undo below the cutoff —
that is what a retention window means. Use --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		retention, _ := cmd.Flags().GetDuration("retention")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}

		report, err := services.RunGC(context.Background(), project.ID, retention, dryRun)
		if err != nil {
			return fmt.Errorf("gc failed: %w", err)
		}

		verb := "Removed"
		if report.DryRun {
			verb = "Would remove"
		}
		for _, br := range report.Branches {
			if len(br.Cutoffs) == 0 {
				continue
			}
			fmt.Printf("Branch %s:\n", br.BranchName)
			for coll, cutoff := range br.Cutoffs {
				fmt.Printf("  %-24s reclaim up to LSN %d\n", coll, cutoff)
			}
		}
		if report.DryRun {
			fmt.Printf("%s entries below the cutoffs above (dry run).\n", verb)
		} else {
			fmt.Printf("%s %d entries.\n", verb, report.EntriesRemoved)
		}
		return nil
	},
}

func init() {
	gcCmd.Flags().StringP("project", "p", "", "Project name (required)")
	gcCmd.Flags().Duration("retention", 7*24*time.Hour, "Retention window for historical reads")
	gcCmd.Flags().Bool("dry-run", false, "Report what would be deleted without deleting")
	rootCmd.AddCommand(gcCmd)
}
