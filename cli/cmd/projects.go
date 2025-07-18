package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"argon-cli/internal/api"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Project represents a MongoDB project for CLI display (matches Neon format)
type Project struct {
	ID          string            `json:"id" yaml:"id"`
	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	CreatedAt   time.Time         `json:"created_at" yaml:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at" yaml:"updated_at"`
	Status      string            `json:"status" yaml:"status"`
	BranchCount int               `json:"branch_count" yaml:"branch_count"`
	StorageSize int64             `json:"storage_size" yaml:"storage_size"`
}

// convertFromAPI converts API project to CLI project format
func convertFromAPI(apiProject *api.Project) *Project {
	return &Project{
		ID:          apiProject.ID,
		Name:        apiProject.Name,
		Description: apiProject.Description,
		CreatedAt:   apiProject.CreatedAt,
		UpdatedAt:   apiProject.UpdatedAt,
		Status:      apiProject.Status,
		BranchCount: apiProject.BranchCount,
		StorageSize: apiProject.StorageSize,
	}
}

// convertFromAPIList converts API project list to CLI project list
func convertFromAPIList(apiProjects []api.Project) []*Project {
	projects := make([]*Project, len(apiProjects))
	for i, p := range apiProjects {
		projects[i] = convertFromAPI(&p)
	}
	return projects
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
		client := getAPIClient()
		
		projects, err := client.ListProjects()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error listing projects: %v\n", err)
			os.Exit(1)
		}

		cliProjects := convertFromAPIList(projects)
		
		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(cliProjects)
		case "yaml":
			outputYAML(cliProjects)
		default:
			outputProjectsTable(cliProjects)
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
		description, _ := cmd.Flags().GetString("description")
		mongodbURI, _ := cmd.Flags().GetString("mongodb-uri")
		
		if name == "" {
			fmt.Fprintf(os.Stderr, "❌ Project name is required\n")
			os.Exit(1)
		}

		if mongodbURI == "" {
			mongodbURI = "mongodb://localhost:27017/" + name // Default local MongoDB
		}

		client := getAPIClient()
		project, err := client.CreateProject(name, description, mongodbURI)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error creating project: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("✅ Project created successfully!\n")
		fmt.Printf("   ID: %s\n", project.ID)
		fmt.Printf("   Name: %s\n", project.Name)
		fmt.Printf("   Status: %s\n", project.Status)
		fmt.Printf("   Created: %s\n", project.CreatedAt.Format("2006-01-02 15:04:05"))
	},
}

// projectsGetCmd represents the projects get command
var projectsGetCmd = &cobra.Command{
	Use:   "get <project-id>",
	Short: "Get project details",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		projectID := args[0]
		
		client := getAPIClient()
		project, err := client.GetProject(projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error getting project: %v\n", err)
			os.Exit(1)
		}

		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(project)
		case "yaml":
			outputYAML(project)
		default:
			outputProjectsTable([]*Project{convertFromAPI(project)})
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

		client := getAPIClient()
		fmt.Printf("🗑️  Deleting project %s...\n", projectID)
		
		err := client.DeleteProject(projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error deleting project: %v\n", err)
			os.Exit(1)
		}
		
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
	projectsCreateCmd.Flags().StringP("description", "d", "", "Project description")
	projectsCreateCmd.Flags().StringP("mongodb-uri", "u", "", "MongoDB connection URI")
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

func outputProjectsTable(projects []*Project) {
	// Simple table format (in production, use a table library)
	fmt.Printf("%-15s %-20s %-30s %-12s %-8s\n", "ID", "NAME", "DESCRIPTION", "STATUS", "BRANCHES")
	fmt.Printf("%-15s %-20s %-30s %-12s %-8s\n", "---", "----", "-----------", "------", "--------")
	
	for _, project := range projects {
		desc := project.Description
		if len(desc) > 30 {
			desc = desc[:27] + "..."
		}
		fmt.Printf("%-15s %-20s %-30s %-12s %-8d\n", 
			project.ID, project.Name, desc, project.Status, project.BranchCount)
	}
}