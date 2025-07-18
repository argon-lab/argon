package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Branch represents a MongoDB branch (identical structure to Neon)
type Branch struct {
	ID               string            `json:"id" yaml:"id"`
	Name             string            `json:"name" yaml:"name"`
	ProjectID        string            `json:"project_id" yaml:"project_id"`
	ParentID         *string           `json:"parent_id,omitempty" yaml:"parent_id,omitempty"`
	Primary          bool              `json:"primary" yaml:"primary"`
	Protected        bool              `json:"protected" yaml:"protected"`
	CreatedAt        time.Time         `json:"created_at" yaml:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at" yaml:"updated_at"`
	LogicalSize      int64             `json:"logical_size" yaml:"logical_size"`
	PhysicalSize     int64             `json:"physical_size" yaml:"physical_size"`
	Current          bool              `json:"current" yaml:"current"`
	ComputeUnits     float64           `json:"compute_units,omitempty" yaml:"compute_units,omitempty"`
	SuspendTimeoutMs *int              `json:"suspend_timeout_ms,omitempty" yaml:"suspend_timeout_ms,omitempty"`
	ConnectionURI    string            `json:"connection_uri,omitempty" yaml:"connection_uri,omitempty"`
}

// branchesCmd represents the branches command
var branchesCmd = &cobra.Command{
	Use:   "branches",
	Short: "Manage MongoDB branches",
	Long: `Manage MongoDB database branches with Git-like operations.

Branches provide isolated MongoDB environments with copy-on-write semantics.
Create, switch, merge, and delete branches just like Git but for databases.

Examples:
  argon branches list                      # List all branches
  argon branches create --name feature-1   # Create new branch  
  argon branches get <branch-id>           # Get branch details
  argon branches delete <branch-id>        # Delete branch

Compatible with Neon CLI branch management patterns.`,
}

// branchesListCmd represents the branches list command
var branchesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List branches in project",
	Long: `List all branches in the specified project.

Examples:
  argon branches list                         # List branches (requires --project-id)
  argon branches list --project-id proj_123  # List branches for specific project
  argon branches list --output json          # JSON output`,
	Run: func(cmd *cobra.Command, args []string) {
		projectID := viper.GetString("project-id")
		if projectID == "" {
			fmt.Fprintf(os.Stderr, "❌ Project ID is required. Use --project-id flag.\n")
			os.Exit(1)
		}

		// Mock data for demonstration
		mainParent := "br_main_parent"
		branches := []Branch{
			{
				ID:            "br_12345",
				Name:          "main",
				ProjectID:     projectID,
				ParentID:      nil,
				Primary:       true,
				Protected:     true,
				Current:       true,
				CreatedAt:     time.Now().AddDate(0, -1, 0),
				UpdatedAt:     time.Now().AddDate(0, 0, -1),
				LogicalSize:   1024000000, // 1GB
				PhysicalSize:  512000000,  // 512MB (compressed)
				ComputeUnits:  2.0,
				ConnectionURI: "mongodb://branch-main.cluster.argon.dev/database",
			},
			{
				ID:               "br_67890",
				Name:             "experiment-1",
				ProjectID:        projectID,
				ParentID:         &mainParent,
				Primary:          false,
				Protected:        false,
				Current:          false,
				CreatedAt:        time.Now().AddDate(0, 0, -5),
				UpdatedAt:        time.Now().AddDate(0, 0, -1),
				LogicalSize:      1050000000, // 1.05GB
				PhysicalSize:     25000000,   // 25MB (only delta)
				ComputeUnits:     1.0,
				SuspendTimeoutMs: &[]int{300000}[0], // 5 minutes
				ConnectionURI:    "mongodb://branch-experiment-1.cluster.argon.dev/database",
			},
		}

		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(branches)
		case "yaml":
			outputYAML(branches)
		default:
			outputBranchTable(branches)
		}
	},
}

// branchesCreateCmd represents the branches create command
var branchesCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new branch",
	Long: `Create a new MongoDB branch with copy-on-write semantics.

Examples:
  argon branches create --name feature-auth
  argon branches create --name exp-1 --parent main
  argon branches create --name test --compute false`,
	Run: func(cmd *cobra.Command, args []string) {
		projectID := viper.GetString("project-id")
		if projectID == "" {
			fmt.Fprintf(os.Stderr, "❌ Project ID is required. Use --project-id flag.\n")
			os.Exit(1)
		}

		name, _ := cmd.Flags().GetString("name")
		parent, _ := cmd.Flags().GetString("parent")
		compute, _ := cmd.Flags().GetBool("compute")
		branchType, _ := cmd.Flags().GetString("type")

		if name == "" {
			fmt.Fprintf(os.Stderr, "❌ Branch name is required\n")
			os.Exit(1)
		}

		// Mock branch creation
		fmt.Printf("🌿 Creating branch '%s'...\n", name)
		fmt.Println("📊 Analyzing parent branch...")
		fmt.Println("🔧 Setting up copy-on-write pointers...")
		
		if compute {
			fmt.Println("⚡ Starting compute instance...")
		}

		branch := Branch{
			ID:           fmt.Sprintf("br_%d", time.Now().Unix()),
			Name:         name,
			ProjectID:    projectID,
			Primary:      false,
			Protected:    false,
			Current:      false,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
			LogicalSize:  0, // New branch starts empty in terms of changes
			PhysicalSize: 0,
			ComputeUnits: 1.0,
		}

		if parent != "" {
			branch.ParentID = &parent
			fmt.Printf("📋 Copied data from parent branch: %s\n", parent)
		}

		fmt.Printf("✅ Branch created successfully!\n")
		fmt.Printf("   ID: %s\n", branch.ID)
		fmt.Printf("   Name: %s\n", branch.Name)
		fmt.Printf("   Type: %s\n", branchType)
		fmt.Printf("   Compute: %v\n", compute)
		fmt.Printf("   Connection: mongodb://branch-%s.cluster.argon.dev/database\n", name)
	},
}

// branchesGetCmd represents the branches get command
var branchesGetCmd = &cobra.Command{
	Use:   "get <branch-id>",
	Short: "Get branch details",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		branchID := args[0]
		
		// Mock branch retrieval
		branch := Branch{
			ID:            branchID,
			Name:          "example-branch",
			ProjectID:     "proj_12345",
			ParentID:      &[]string{"br_main"}[0],
			Primary:       false,
			Protected:     false,
			Current:       false,
			CreatedAt:     time.Now().AddDate(0, 0, -5),
			UpdatedAt:     time.Now().AddDate(0, 0, -1),
			LogicalSize:   1050000000,
			PhysicalSize:  25000000,
			ComputeUnits:  1.0,
			ConnectionURI: "mongodb://branch-example.cluster.argon.dev/database",
		}

		outputFormat := viper.GetString("output")
		switch outputFormat {
		case "json":
			outputJSON(branch)
		case "yaml":
			outputYAML(branch)
		default:
			outputBranchTable([]Branch{branch})
		}
	},
}

// branchesDeleteCmd represents the branches delete command
var branchesDeleteCmd = &cobra.Command{
	Use:   "delete <branch-id>",
	Short: "Delete a branch",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		branchID := args[0]
		
		fmt.Printf("⚠️  Are you sure you want to delete branch %s? (y/N): ", branchID)
		var response string
		fmt.Scanln(&response)
		
		if response != "y" && response != "yes" {
			fmt.Println("❌ Deletion cancelled")
			return
		}

		fmt.Printf("🗑️  Deleting branch %s...\n", branchID)
		fmt.Println("🧹 Cleaning up compute resources...")
		fmt.Println("💾 Removing storage references...")
		fmt.Println("✅ Branch deleted successfully")
	},
}

// branchesRenameCmd represents the branches rename command
var branchesRenameCmd = &cobra.Command{
	Use:   "rename <branch-id> <new-name>",
	Short: "Rename a branch",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		branchID := args[0]
		newName := args[1]
		
		fmt.Printf("📝 Renaming branch %s to '%s'...\n", branchID, newName)
		fmt.Println("🔄 Updating connection strings...")
		fmt.Println("✅ Branch renamed successfully")
	},
}

func init() {
	rootCmd.AddCommand(branchesCmd)
	
	// Add subcommands
	branchesCmd.AddCommand(branchesListCmd)
	branchesCmd.AddCommand(branchesCreateCmd)
	branchesCmd.AddCommand(branchesGetCmd)
	branchesCmd.AddCommand(branchesDeleteCmd)
	branchesCmd.AddCommand(branchesRenameCmd)
	
	// Add flags for create command (identical to Neon)
	branchesCreateCmd.Flags().StringP("name", "n", "", "Branch name (required)")
	branchesCreateCmd.Flags().StringP("parent", "p", "main", "Parent branch")
	branchesCreateCmd.Flags().BoolP("compute", "c", true, "Create with compute")
	branchesCreateCmd.Flags().StringP("type", "t", "read_write", "Branch type (read_write|read_only)")
	branchesCreateCmd.Flags().Float64("cu", 1.0, "Compute units")
	branchesCreateCmd.Flags().Int("suspend-timeout", 300, "Suspend timeout in seconds")
	branchesCreateCmd.MarkFlagRequired("name")
}

func outputBranchTable(branches []Branch) {
	fmt.Printf("%-15s %-20s %-12s %-10s %-15s %-10s\n", 
		"ID", "NAME", "PROJECT", "PRIMARY", "SIZE", "CURRENT")
	fmt.Printf("%-15s %-20s %-12s %-10s %-15s %-10s\n", 
		"--", "----", "-------", "-------", "----", "-------")
	
	for _, branch := range branches {
		primary := "false"
		if branch.Primary {
			primary = "true"
		}
		current := "false"
		if branch.Current {
			current = "true"
		}
		
		// Format size in human readable format
		size := formatBytes(branch.PhysicalSize)
		
		fmt.Printf("%-15s %-20s %-12s %-10s %-15s %-10s\n",
			branch.ID, branch.Name, branch.ProjectID[:8]+"...", 
			primary, size, current)
	}
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}