package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Project represents a MongoDB project (identical structure to Neon)
type Project struct {
	ID          string            `json:"id" yaml:"id"`
	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	CreatedAt   time.Time         `json:"created_at" yaml:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at" yaml:"updated_at"`
	OwnerID     string            `json:"owner_id" yaml:"owner_id"`
	RegionID    string            `json:"region_id" yaml:"region_id"`
	Settings    map[string]string `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// projectsCmd represents the projects command
var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Manage Argon projects",
	Long: `Manage MongoDB projects in Argon.

Projects are containers for your MongoDB databases and branches.
Each project corresponds to a MongoDB cluster with branching capabilities.

Examples:
  argon projects list                    # List all projects
  argon projects create --name my-app    # Create new project
  argon projects get <project-id>        # Get project details
  argon projects delete <project-id>     # Delete project

Compatible with Neon CLI project management patterns.`,
}

// projectsListCmd represents the projects list command  
var projectsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all projects",
	Long: `List all projects in your Argon account.

Examples:
  argon projects list                    # Default table output
  argon projects list --output json     # JSON output
  argon projects list --output yaml     # YAML output`,
	Run: func(cmd *cobra.Command, args []string) {
		// Mock data for demonstration (in real implementation, call API)
		projects := []Project{
			{
				ID:          "proj_12345",
				Name:        "ml-experiments",
				Description: "Machine learning experiments database",
				CreatedAt:   time.Now().AddDate(0, -1, 0),
				UpdatedAt:   time.Now().AddDate(0, 0, -1),
				OwnerID:     "user_67890",
				RegionID:    "us-east-1",
				Settings:    map[string]string{"tier": "pro"},
			},
			{
				ID:          "proj_54321", 
				Name:        "production-app",
				Description: "Production application database",
				CreatedAt:   time.Now().AddDate(0, -2, 0),
				UpdatedAt:   time.Now().AddDate(0, 0, -3),
				OwnerID:     "user_67890",
				RegionID:    "us-west-2",
				Settings:    map[string]string{"tier": "enterprise"},
			},
		}

		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(projects)
		case "yaml":
			outputYAML(projects)
		default:
			outputTable(projects)
		}
	},
}

// projectsCreateCmd represents the projects create command
var projectsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new project",
	Long: `Create a new MongoDB project in Argon.

Examples:
  argon projects create --name my-app
  argon projects create --name ml-project --region us-west-2`,
	Run: func(cmd *cobra.Command, args []string) {
		name, _ := cmd.Flags().GetString("name")
		region, _ := cmd.Flags().GetString("region")
		
		if name == "" {
			fmt.Fprintf(os.Stderr, "❌ Project name is required\n")
			os.Exit(1)
		}

		// Mock project creation
		project := Project{
			ID:          fmt.Sprintf("proj_%d", time.Now().Unix()),
			Name:        name,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			OwnerID:     "user_current",
			RegionID:    region,
		}

		fmt.Printf("✅ Project created successfully!\n")
		fmt.Printf("   ID: %s\n", project.ID)
		fmt.Printf("   Name: %s\n", project.Name)
		fmt.Printf("   Region: %s\n", project.RegionID)
	},
}

// projectsGetCmd represents the projects get command
var projectsGetCmd = &cobra.Command{
	Use:   "get <project-id>",
	Short: "Get project details",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		projectID := args[0]
		
		// Mock project retrieval
		project := Project{
			ID:          projectID,
			Name:        "example-project",
			Description: "Example MongoDB project with branching",
			CreatedAt:   time.Now().AddDate(0, -1, 0),
			UpdatedAt:   time.Now().AddDate(0, 0, -1),
			OwnerID:     "user_67890",
			RegionID:    "us-east-1",
			Settings:    map[string]string{"tier": "pro", "compute_units": "2"},
		}

		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(project)
		case "yaml":
			outputYAML(project)
		default:
			outputTable([]Project{project})
		}
	},
}

// projectsDeleteCmd represents the projects delete command
var projectsDeleteCmd = &cobra.Command{
	Use:   "delete <project-id>",
	Short: "Delete a project",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		projectID := args[0]
		
		fmt.Printf("⚠️  Are you sure you want to delete project %s? (y/N): ", projectID)
		var response string
		fmt.Scanln(&response)
		
		if response != "y" && response != "yes" {
			fmt.Println("❌ Deletion cancelled")
			return
		}

		// Mock deletion
		fmt.Printf("🗑️  Deleting project %s...\n", projectID)
		fmt.Println("✅ Project deleted successfully")
	},
}

func init() {
	rootCmd.AddCommand(projectsCmd)
	
	// Add subcommands
	projectsCmd.AddCommand(projectsListCmd)
	projectsCmd.AddCommand(projectsCreateCmd)
	projectsCmd.AddCommand(projectsGetCmd)
	projectsCmd.AddCommand(projectsDeleteCmd)
	
	// Add flags for create command
	projectsCreateCmd.Flags().StringP("name", "n", "", "Project name (required)")
	projectsCreateCmd.Flags().StringP("region", "r", "us-east-1", "AWS region")
	projectsCreateCmd.MarkFlagRequired("name")
}

// Output formatting functions (identical to Neon CLI patterns)

func outputJSON(data interface{}) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		os.Exit(1)
	}
}

func outputYAML(data interface{}) {
	encoder := yaml.NewEncoder(os.Stdout)
	encoder.SetIndent(2)
	if err := encoder.Encode(data); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding YAML: %v\n", err)
		os.Exit(1)
	}
}

func outputTable(projects []Project) {
	// Simple table format (in production, use a table library)
	fmt.Printf("%-15s %-20s %-30s %-12s\n", "ID", "NAME", "DESCRIPTION", "REGION")
	fmt.Printf("%-15s %-20s %-30s %-12s\n", "---", "----", "-----------", "------")
	
	for _, project := range projects {
		desc := project.Description
		if len(desc) > 30 {
			desc = desc[:27] + "..."
		}
		fmt.Printf("%-15s %-20s %-30s %-12s\n", 
			project.ID, project.Name, desc, project.RegionID)
	}
}