package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

// resolveProjectID maps a --project value to the project ID: by name
// first, falling back to treating the value as an ID for scripts that
// pass one directly.
func resolveProjectID(services *walcli.Services, projectName string) (string, error) {
	if project, err := services.Projects.GetProjectByName(projectName); err == nil {
		return project.ID, nil
	}
	if project, err := services.Projects.GetProject(projectName); err == nil {
		return project.ID, nil
	}
	return "", fmt.Errorf("project %q not found", projectName)
}

// resolveBranch loads the project and branch named by the shared flags.
func resolveBranch(services *walcli.Services, projectName, branchName string) (string, error) {
	project, err := services.Projects.GetProjectByName(projectName)
	if err != nil {
		return "", fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if branchName == "" {
		branchName = "main"
	}
	branch, err := services.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		return "", fmt.Errorf("branch %q not found: %w", branchName, err)
	}
	return branch.ID, nil
}

var checkoutCmd = &cobra.Command{
	Use:   "checkout",
	Short: "Materialize a branch into a real MongoDB database",
	Long: `Checkout builds the branch's state into a physical MongoDB database
that any unmodified MongoDB driver can connect to: queries, indexes,
aggregation and transactions all run on mongod itself. While checked
out, feed the WAL by running "argon watch" so direct writes keep
versioned history; SDK writes to a checked-out branch are rejected.

Repeating checkout on a live branch returns its existing database and
preserves pending direct writes. Stop writers before release.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}

		info, err := services.Checkout.Checkout(context.Background(), branchID)
		if err != nil {
			return fmt.Errorf("checkout failed: %w", err)
		}

		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branchID, "physical_db": info.PhysicalDB, "lsn": info.LSN, "connection_string": services.BranchConnectionString(info.PhysicalDB), "capture_managed": false})
		}
		fmt.Printf("Checked out at LSN %d: %d collection(s), %d document(s)\n",
			info.LSN, info.Collections, info.Documents)
		fmt.Printf("Connection string:\n  %s\n", services.BranchConnectionString(info.PhysicalDB))
		fmt.Println("Run \"argon watch\" for this branch to capture direct writes into the WAL.")
		return nil
	},
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Print the connection string of a checked-out branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		branch, err := services.Branches.GetBranchByID(branchID)
		if err != nil {
			return err
		}
		if !branch.IsLive() {
			return fmt.Errorf("branch is not checked out; run \"argon checkout\" first")
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branchID, "connection_string": services.BranchConnectionString(branch.PhysicalDB), "capture_managed": false})
		}
		fmt.Println(services.BranchConnectionString(branch.PhysicalDB))
		return nil
	},
}

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Drop a branch's physical database (the WAL keeps the history)",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		if err := services.Checkout.Release(context.Background(), branchID); err != nil {
			return fmt.Errorf("release failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branchID, "released": true})
		}
		fmt.Println("Released. Check the branch out again anytime to rebuild it from the WAL.")
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{checkoutCmd, connectCmd, releaseCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		c.Flags().StringP("branch", "b", "", "Branch name (default: main)")
	}
	rootCmd.AddCommand(checkoutCmd)
	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(releaseCmd)
}
