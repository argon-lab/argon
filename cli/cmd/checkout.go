package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

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

Re-running checkout refreshes the database to the branch's current WAL
state.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := walcli.NewServices()
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

		services, err := walcli.NewServices()
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

		services, err := walcli.NewServices()
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
