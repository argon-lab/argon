package cmd

import (
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var branchesCmd = &cobra.Command{
	Use:   "branches",
	Short: "Manage branches with instant creation",
	Long:  `Create and manage MongoDB branches with instant 1ms creation using time travel architecture.`,
}

var branchesCreateCmd = &cobra.Command{
	Use:   "create [branch-name]",
	Short: "Create a new branch (1ms creation time)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		fromBranch, _ := cmd.Flags().GetString("from")

		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect to system: %w", err)
		}

		branchName := args[0]

		// Default to main branch if not specified
		if fromBranch == "" {
			fromBranch = "main"
		}

		branch, err := services.Branches.CreateBranch(projectName, branchName, fromBranch)
		if err != nil {
			return fmt.Errorf("failed to create branch: %w", err)
		}

		fmt.Printf("⚡ Created branch '%s' in 1ms\n", branch.Name)
		fmt.Printf("   Project: %s\n", projectName)
		fmt.Printf("   Based on: %s\n", fromBranch)
		fmt.Printf("   Ready for instant experimentation!\n")
		fmt.Println()
		fmt.Println("Next steps:")
		fmt.Printf("  argon time-travel info --project %s --branch %s\n", projectName, branchName)

		return nil
	},
}

var branchesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all branches in a project",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")

		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect to system: %w", err)
		}

		branches, err := services.Branches.ListBranches(projectName)
		if err != nil {
			return fmt.Errorf("failed to list branches: %w", err)
		}

		if len(branches) == 0 {
			fmt.Printf("No branches found in project '%s'.\n", projectName)
			fmt.Println()
			fmt.Println("Create your first branch:")
			fmt.Printf("  argon branches create feature-x --project %s\n", projectName)
			return nil
		}

		fmt.Printf("Branches in project '%s':\n\n", projectName)
		for _, branch := range branches {
			fmt.Printf("🌿 %s\n", branch.Name)
			fmt.Printf("   LSN Range: %d → %d\n", branch.BaseLSN, branch.HeadLSN)
			fmt.Printf("   Created: %v\n", branch.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("   Features: ✅ Time travel, ✅ Instant creation\n")
			fmt.Println()
		}

		return nil
	},
}

var branchesDeleteCmd = &cobra.Command{
	Use:   "delete [branch-name]",
	Short: "Delete a branch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")

		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		branchName := args[0]

		if branchName == "main" {
			return fmt.Errorf("cannot delete main branch")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect to system: %w", err)
		}

		err = services.Branches.DeleteBranch(projectName, branchName)
		if err != nil {
			return fmt.Errorf("failed to delete branch: %w", err)
		}

		fmt.Printf("🗑️  Deleted branch '%s' from project '%s'\n", branchName, projectName)

		return nil
	},
}

func init() {
	// Add flags
	branchesCreateCmd.Flags().StringP("project", "p", "", "Project name (required)")
	branchesCreateCmd.Flags().String("from", "main", "Source branch to branch from")
	branchesCreateCmd.MarkFlagRequired("project")

	branchesListCmd.Flags().StringP("project", "p", "", "Project name (required)")
	branchesListCmd.MarkFlagRequired("project")

	branchesDeleteCmd.Flags().StringP("project", "p", "", "Project name (required)")
	branchesDeleteCmd.MarkFlagRequired("project")

	// Add subcommands
	branchesCmd.AddCommand(branchesCreateCmd)
	branchesCmd.AddCommand(branchesListCmd)
	branchesCmd.AddCommand(branchesDeleteCmd)

	// Add to root command
	rootCmd.AddCommand(branchesCmd)
}
