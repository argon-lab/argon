package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Manage Argon projects with time travel",
	Long:  `Create and manage MongoDB projects with instant branching and time travel capabilities.`,
}

var projectsCreateCmd = &cobra.Command{
	Use:   "create [project-name]",
	Short: "Create a new project with time travel",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect to system: %w", err)
		}

		projectName := args[0]
		project, err := services.Projects.CreateProject(projectName)
		if err != nil {
			return fmt.Errorf("failed to create project: %w", err)
		}

		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"project": project})
		}

		fmt.Printf("✅ Created project '%s' with time travel capabilities\n", project.Name)
		fmt.Printf("   Project ID: %s\n", project.ID)
		fmt.Printf("   Main branch created with instant branching\n")
		fmt.Println()
		fmt.Println("Next steps:")
		fmt.Printf("  argon branches list --project %s\n", project.Name)
		fmt.Printf("  argon time-travel info --project %s\n", project.Name)

		return nil
	},
}

var projectsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect to system: %w", err)
		}

		projects, err := services.Projects.ListProjects()
		if err != nil {
			return fmt.Errorf("failed to list projects: %w", err)
		}

		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"projects": projects})
		}
		if len(projects) == 0 {
			fmt.Println("No projects found.")
			fmt.Println()
			fmt.Println("Create your first project with time travel:")
			fmt.Println("  argon projects create my-project")
			return nil
		}

		fmt.Printf("Found %d project(s) with time travel:\n\n", len(projects))
		for _, project := range projects {
			fmt.Printf("📁 %s\n", project.Name)
			fmt.Printf("   ID: %s\n", project.ID)
			fmt.Printf("   Created: %v\n", project.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("   Features: ✅ Instant branching, ✅ Time travel\n")
			fmt.Println()
		}

		return nil
	},
}

func init() {
	// Add subcommands
	projectsCmd.AddCommand(projectsCreateCmd)
	projectsCmd.AddCommand(projectsListCmd)

	// Add to root command
	rootCmd.AddCommand(projectsCmd)
}
