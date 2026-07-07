package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage collection snapshots",
	Long: `Snapshots capture the materialized state of a branch at an LSN so
that reads replay only the delta above the snapshot instead of the
branch's entire history. Argon also takes snapshots automatically as
branches grow; these commands trigger and inspect them explicitly.`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Snapshot a branch at its current head",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if branchName == "" {
			branchName = "main"
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		branch, err := services.Branches.GetBranch(project.ID, branchName)
		if err != nil {
			return fmt.Errorf("branch %q not found: %w", branchName, err)
		}

		snaps, err := services.Snapshots.CreateSnapshot(context.Background(), branch.ID, branch.HeadLSN)
		if err != nil {
			return fmt.Errorf("snapshot failed: %w", err)
		}

		fmt.Printf("Created %d collection snapshot(s) at LSN %d:\n", len(snaps), branch.HeadLSN)
		for _, s := range snaps {
			fmt.Printf("  %-24s %6d docs  %8d bytes  %d chunk(s)\n",
				s.Collection, s.DocCount, s.SizeBytes, len(s.ChunkIDs))
		}
		return nil
	},
}

var snapshotListCmd = &cobra.Command{
	Use:   "list",
	Short: "List snapshots of a branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if branchName == "" {
			branchName = "main"
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		branch, err := services.Branches.GetBranch(project.ID, branchName)
		if err != nil {
			return fmt.Errorf("branch %q not found: %w", branchName, err)
		}

		snaps, err := services.Snapshots.ListSnapshots(context.Background(), branch.ID)
		if err != nil {
			return fmt.Errorf("failed to list snapshots: %w", err)
		}
		if len(snaps) == 0 {
			fmt.Println("No snapshots yet.")
			return nil
		}

		fmt.Printf("%-8s %-24s %8s %10s %s\n", "LSN", "COLLECTION", "DOCS", "BYTES", "CREATED")
		for _, s := range snaps {
			fmt.Printf("%-8d %-24s %8d %10d %s\n",
				s.LSN, s.Collection, s.DocCount, s.SizeBytes, s.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{snapshotCreateCmd, snapshotListCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		c.Flags().StringP("branch", "b", "", "Branch name (default: main)")
	}
	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	rootCmd.AddCommand(snapshotCmd)
}
