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

		// Default to main branch if not specified
		if branchID == "" {
			branchID = "main"
		}

		// Default database name
		if database == "" {
			database = "database"
		}

		// Default role
		if role == "" {
			role = "user"
		}

		// Generate MongoDB connection string
		// In a real implementation, this would fetch from the API
		host := fmt.Sprintf("branch-%s.cluster.argon.dev", branchID)
		connectionString := fmt.Sprintf("mongodb://%s:<password>@%s/%s", role, host, database)

		fmt.Printf("📋 MongoDB Connection String:\n")
		fmt.Printf("   %s\n\n", connectionString)
		
		fmt.Printf("🔧 Connection Details:\n")
		fmt.Printf("   Project ID: %s\n", projectID)
		fmt.Printf("   Branch: %s\n", branchID)
		fmt.Printf("   Database: %s\n", database)
		fmt.Printf("   Role: %s\n", role)
		fmt.Printf("   Host: %s\n", host)
		
		fmt.Printf("\n💡 Usage Examples:\n")
		fmt.Printf("   # MongoDB shell\n")
		fmt.Printf("   mongosh \"%s\"\n\n", connectionString)
		
		fmt.Printf("   # Python (PyMongo)\n")
		fmt.Printf("   from pymongo import MongoClient\n")
		fmt.Printf("   client = MongoClient(\"%s\")\n\n", connectionString)
		
		fmt.Printf("   # Node.js (MongoDB Driver)\n")
		fmt.Printf("   const { MongoClient } = require('mongodb');\n")
		fmt.Printf("   const client = new MongoClient(\"%s\");\n\n", connectionString)
		
		fmt.Printf("⚠️  Remember to replace <password> with your actual password!\n")
	},
}

func init() {
	rootCmd.AddCommand(connectionStringCmd)
	
	// Add flags
	connectionStringCmd.Flags().StringP("branch-id", "b", "", "Branch ID (defaults to main)")
	connectionStringCmd.Flags().StringP("database", "d", "", "Database name")
	connectionStringCmd.Flags().StringP("role", "r", "", "Database role/user")
}