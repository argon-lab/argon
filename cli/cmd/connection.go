package cmd

import (
	"fmt"
	"os"


	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// connectionStringCmd represents the connection-string command
var connectionStringCmd = &cobra.Command{
	Use:   "connection-string",
	Short: "Get MongoDB connection string",
	Long: `Get the MongoDB connection string for the specified branch.

Examples:
  argon connection-string                            # Get connection for current branch
  argon connection-string --project-id proj_123     # Specify project
  argon connection-string --branch-id br_456        # Specify branch
  argon connection-string --database mydb           # Include database name

Compatible with Neon CLI connection string patterns.`,
	Run: func(cmd *cobra.Command, args []string) {
		projectID := viper.GetString("project-id")
		branchID, _ := cmd.Flags().GetString("branch-id")
		database, _ := cmd.Flags().GetString("database")
		role, _ := cmd.Flags().GetString("role")
		
		if projectID == "" {
			fmt.Fprintf(os.Stderr, "❌ Project ID is required. Use --project-id flag.\n")
			os.Exit(1)
		}

		client := getAPIClient()

		// If no branch ID specified, find the main branch
		if branchID == "" {
			branches, err := client.ListBranches(projectID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error listing branches: %v\n", err)
				os.Exit(1)
			}
			
			// Find main branch
			for _, branch := range branches {
				if branch.IsMain {
					branchID = branch.ID
					break
				}
			}
			
			if branchID == "" {
				fmt.Fprintf(os.Stderr, "❌ No main branch found for project %s\n", projectID)
				os.Exit(1)
			}
		}

		// Get the connection string from API
		connStr, err := client.GetConnectionString(projectID, branchID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error getting connection string: %v\n", err)
			os.Exit(1)
		}

		// Get branch details for display
		branch, err := client.GetBranch(projectID, branchID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error getting branch details: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("📋 MongoDB Connection String:\n")
		fmt.Printf("   %s\n\n", connStr.ConnectionString)
		
		fmt.Printf("🔧 Connection Details:\n")
		fmt.Printf("   Project ID: %s\n", projectID)
		fmt.Printf("   Branch: %s (%s)\n", branch.Name, branch.ID)
		fmt.Printf("   Database: %s\n", connStr.DatabaseName)
		fmt.Printf("   Status: %s\n", branch.Status)
		if connStr.ExpiresAt != nil {
			fmt.Printf("   Expires: %s\n", connStr.ExpiresAt.Format("2006-01-02 15:04:05 UTC"))
		}
		
		fmt.Printf("\n💡 Usage Examples:\n")
		fmt.Printf("   # MongoDB shell\n")
		fmt.Printf("   mongosh \"%s\"\n\n", connStr.ConnectionString)
		
		fmt.Printf("   # Python (PyMongo)\n")
		fmt.Printf("   from pymongo import MongoClient\n")
		fmt.Printf("   client = MongoClient(\"%s\")\n\n", connStr.ConnectionString)
		
		fmt.Printf("   # Node.js (MongoDB Driver)\n")
		fmt.Printf("   const { MongoClient } = require('mongodb');\n")
		fmt.Printf("   const client = new MongoClient(\"%s\");\n\n", connStr.ConnectionString)
		
		if role != "" || database != "" {
			fmt.Printf("⚠️  Custom role and database flags are applied at the application level.\n")
		}
	},
}

func init() {
	rootCmd.AddCommand(connectionStringCmd)
	
	// Add flags
	connectionStringCmd.Flags().StringP("branch-id", "b", "", "Branch ID (defaults to main)")
	connectionStringCmd.Flags().StringP("database", "d", "", "Database name")
	connectionStringCmd.Flags().StringP("role", "r", "", "Database role/user")
}