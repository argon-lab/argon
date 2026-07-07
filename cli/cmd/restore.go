package cmd

import (
	"fmt"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Rewind a branch or fork history into a new branch",
	Long: `Restore rewinds a branch's head to a historical point (by LSN or
RFC3339 time), or forks the historical state into a new branch without
touching the original. Resets are recorded, never destructive: the
discarded entries stay in the WAL for audit, and branches forked from
the pre-reset head (or pins on it) keep reading the old state.`,
}

// restoreTarget resolves the --lsn/--time flags to a concrete LSN.
func restoreTarget(cmd *cobra.Command, services *walcli.Services, branchID string) (int64, error) {
	lsn, _ := cmd.Flags().GetInt64("lsn")
	atTime, _ := cmd.Flags().GetString("time")
	if (lsn == 0) == (atTime == "") {
		return 0, fmt.Errorf("exactly one of --lsn or --time is required")
	}
	if lsn != 0 {
		return lsn, nil
	}
	t, err := time.Parse(time.RFC3339, atTime)
	if err != nil {
		return 0, fmt.Errorf("invalid --time (want RFC3339, e.g. 2026-07-07T12:00:00Z): %w", err)
	}
	branch, err := services.Branches.GetBranchByID(branchID)
	if err != nil {
		return 0, err
	}
	target, err := services.TimeTravel.FindLSNAtTime(branch, t)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve time to LSN: %w", err)
	}
	return target, nil
}

var restorePreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Show what a reset would discard",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		target, err := restoreTarget(cmd, services, branchID)
		if err != nil {
			return err
		}

		preview, err := services.Restore.GetRestorePreview(branchID, target)
		if err != nil {
			return err
		}
		fmt.Printf("Branch:  %s (head LSN %d)\n", preview.BranchName, preview.CurrentLSN)
		fmt.Printf("Target:  LSN %d\n", preview.TargetLSN)
		fmt.Printf("Discards %d operation(s)\n", preview.OperationsToDiscard)
		for collection, count := range preview.AffectedCollections {
			fmt.Printf("  %-24s %d\n", collection, count)
		}
		fmt.Println("Discarded entries stay in the WAL for audit; a reset is recorded, not destructive.")
		return nil
	},
}

var restoreResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Rewind the branch head to a historical point",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		backup, _ := cmd.Flags().GetString("backup")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		target, err := restoreTarget(cmd, services, branchID)
		if err != nil {
			return err
		}

		if backup != "" {
			branch, err := services.Branches.GetBranchByID(branchID)
			if err != nil {
				return err
			}
			if _, err := services.Restore.CreateBranchAtLSN(project.ID, branchID, backup, branch.HeadLSN); err != nil {
				return fmt.Errorf("failed to create backup branch: %w", err)
			}
			fmt.Printf("Backup branch %q created at LSN %d\n", backup, branch.HeadLSN)
		}

		branch, err := services.Restore.ResetBranchToLSN(branchID, target)
		if err != nil {
			return err
		}
		fmt.Printf("Reset %s to LSN %d\n", branch.Name, branch.HeadLSN)
		if branch.IsLive() {
			fmt.Println("The branch is checked out: run \"argon checkout\" again to refresh the physical database.")
		}
		return nil
	},
}

var restoreBranchCmd = &cobra.Command{
	Use:   "branch",
	Short: "Fork the historical state into a new branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		as, _ := cmd.Flags().GetString("as")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		target, err := restoreTarget(cmd, services, branchID)
		if err != nil {
			return err
		}

		branch, err := services.Restore.CreateBranchAtLSN(project.ID, branchID, as, target)
		if err != nil {
			return err
		}
		fmt.Printf("Created branch %q at LSN %d\n", branch.Name, branch.HeadLSN)
		return nil
	},
}

func addRestoreTargetFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("project", "p", "", "Project name (required)")
	cmd.Flags().StringP("branch", "b", "main", "Branch to restore")
	cmd.Flags().Int64("lsn", 0, "Target LSN")
	cmd.Flags().String("time", "", "Target RFC3339 time (alternative to --lsn)")
	_ = cmd.MarkFlagRequired("project")
}

func init() {
	addRestoreTargetFlags(restorePreviewCmd)
	addRestoreTargetFlags(restoreResetCmd)
	restoreResetCmd.Flags().String("backup", "", "Fork this backup branch at the current head before resetting")
	addRestoreTargetFlags(restoreBranchCmd)
	restoreBranchCmd.Flags().String("as", "", "Name for the new branch (required)")
	_ = restoreBranchCmd.MarkFlagRequired("as")

	restoreCmd.AddCommand(restorePreviewCmd, restoreResetCmd, restoreBranchCmd)
	rootCmd.AddCommand(restoreCmd)
}
