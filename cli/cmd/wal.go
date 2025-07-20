package cmd

import (
	"fmt"

	"github.com/argon-lab/argon/pkg/config"
	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var walCmd = &cobra.Command{
	Use:   "wal",
	Short: "WAL-based operations (experimental)",
	Long:  `Manage projects and branches using the new WAL-based architecture.`,
}

var walProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage WAL projects",
}

var walBranchCmd = &cobra.Command{
	Use:   "branch",
	Short: "Manage WAL branches",
}

var walCreateProjectCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new WAL-enabled project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		project, err := services.Projects.CreateProject(args[0])
		if err != nil {
			return fmt.Errorf("failed to create project: %w", err)
		}

		fmt.Printf("Created WAL-enabled project '%s' (ID: %s)\n", project.Name, project.ID)
		return nil
	},
}

var walListProjectsCmd = &cobra.Command{
	Use:   "list",
	Short: "List WAL-enabled projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		projects, err := services.Projects.ListProjects()
		if err != nil {
			return fmt.Errorf("failed to list projects: %w", err)
		}

		if len(projects) == 0 {
			fmt.Println("No WAL-enabled projects found")
			return nil
		}

		fmt.Println("WAL-Enabled Projects:")
		for _, project := range projects {
			fmt.Printf("  - %s (ID: %s)\n", project.Name, project.ID)
		}

		return nil
	},
}

var walCreateBranchCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new WAL branch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		projectID, _ := cmd.Flags().GetString("project")
		parentBranch, _ := cmd.Flags().GetString("from")

		if projectID == "" {
			return fmt.Errorf("project ID is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		// Get parent branch ID if specified
		parentID := ""
		if parentBranch != "" {
			parent, err := services.Branches.GetBranch(projectID, parentBranch)
			if err != nil {
				return fmt.Errorf("parent branch not found: %w", err)
			}
			parentID = parent.ID
		}

		branch, err := services.Branches.CreateBranch(projectID, args[0], parentID)
		if err != nil {
			return fmt.Errorf("failed to create branch: %w", err)
		}

		fmt.Printf("Created WAL branch '%s' (LSN: %d, Base: %d)\n", 
			branch.Name, branch.HeadLSN, branch.BaseLSN)
		return nil
	},
}

var walListBranchesCmd = &cobra.Command{
	Use:   "list",
	Short: "List WAL branches",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		projectID, _ := cmd.Flags().GetString("project")
		if projectID == "" {
			return fmt.Errorf("project ID is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		branches, err := services.Branches.ListBranches(projectID)
		if err != nil {
			return fmt.Errorf("failed to list branches: %w", err)
		}

		if len(branches) == 0 {
			fmt.Println("No branches found")
			return nil
		}

		fmt.Printf("WAL Branches for project %s:\n", projectID)
		for _, branch := range branches {
			fmt.Printf("  - %s (Head LSN: %d, Base LSN: %d)\n", 
				branch.Name, branch.HeadLSN, branch.BaseLSN)
		}

		return nil
	},
}

var walStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show WAL system status",
	RunE: func(cmd *cobra.Command, args []string) error {
		features := config.GetFeatures()

		fmt.Println("WAL System Status:")
		fmt.Printf("  Enabled: %v\n", features.EnableWAL)
		fmt.Printf("  New Projects Use WAL: %v\n", features.WALForNewProjects)
		fmt.Printf("  New Branches Use WAL: %v\n", features.WALForNewBranches)
		fmt.Printf("  Migration Enabled: %v\n", features.WALMigrationEnabled)

		if !features.EnableWAL {
			fmt.Println("\nTo enable WAL, set environment variable: ENABLE_WAL=true")
		}

		return nil
	},
}


func init() {
	// Add subcommands
	walProjectCmd.AddCommand(walCreateProjectCmd)
	walProjectCmd.AddCommand(walListProjectsCmd)

	walBranchCmd.AddCommand(walCreateBranchCmd)
	walBranchCmd.AddCommand(walListBranchesCmd)

	// Add flags
	walCreateBranchCmd.Flags().StringP("project", "p", "", "Project ID")
	walCreateBranchCmd.Flags().StringP("from", "f", "", "Parent branch name")

	walListBranchesCmd.Flags().StringP("project", "p", "", "Project ID")

	// Add to WAL command
	walCmd.AddCommand(walProjectCmd)
	walCmd.AddCommand(walBranchCmd)
	walCmd.AddCommand(walStatusCmd)

	// Add to root command
	rootCmd.AddCommand(walCmd)
}