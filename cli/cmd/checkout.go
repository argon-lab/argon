package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/v2/pkg/walcli"
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
	branch, err := services.ResolveBranch(projectName, branchName)
	if err != nil {
		return "", err
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
		if info.SandboxUsername != "" {
			return walcli.ErrSandboxURIOnce
		}

		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branchID, "physical_db": info.PhysicalDB, "lsn": info.LSN, "connection_string": services.BranchConnectionString(info.PhysicalDB), "capture_managed": false, "indexes": info.Indexes, "index_notes": info.IndexNotes})
		}
		fmt.Printf("Checked out at LSN %d: %d collection(s), %d document(s), %d secondary index(es)\n",
			info.LSN, info.Collections, info.Documents, info.Indexes)
		printIndexNotes(cmd, info.IndexNotes)
		fmt.Printf("Connection string:\n  %s\n", services.BranchConnectionString(info.PhysicalDB))
		fmt.Println("Run \"argon watch\" for this branch to capture direct writes into the WAL.")
		return nil
	},
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Print the connection string of a checked-out branch",
	Long: `Print the connection string of a checked-out branch.

The branch record must name the database Argon created for that branch.`,
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
		branch, err := services.ResolveBranch(projectName, branchName)
		if err != nil {
			return err
		}
		if branch.SandboxUsername != "" {
			return walcli.ErrSandboxURIOnce
		}
		if !branch.IsLive() {
			return fmt.Errorf("branch is not checked out; run \"argon checkout\" first")
		}
		own, err := branch.OwnDatabase()
		if err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branch.ID, "connection_string": services.BranchConnectionString(own), "capture_managed": false})
		}
		fmt.Println(services.BranchConnectionString(own))
		return nil
	},
}

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Drop a branch's physical database (the WAL keeps the history)",
	Long: `Release drops the branch's own physical database. Stop its writers first.

If Argon 2.5.0 checked out a followed branch, upgrade every process using
the metadata database first. Releasing that checkout discards any direct
writes that never reached the WAL. Run "argon source resync" after release.`,
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
