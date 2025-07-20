package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/argon-lab/argon/pkg/walcli"
)

// ImportPreview contains information about what would be imported
type ImportPreview struct {
	DatabaseName        string            `json:"database_name"`
	Collections         []CollectionInfo  `json:"collections"`
	TotalDocuments      int64             `json:"total_documents"`
	EstimatedSize       int64             `json:"estimated_size_bytes"`
	EstimatedWALEntries int64             `json:"estimated_wal_entries"`
}

// CollectionInfo contains details about a collection to be imported
type CollectionInfo struct {
	Name          string `json:"name"`
	DocumentCount int64  `json:"document_count"`
	SizeBytes     int64  `json:"size_bytes"`
	IndexCount    int    `json:"index_count"`
}

// ImportOptions configures the import process
type ImportOptions struct {
	MongoURI     string `json:"mongo_uri"`
	DatabaseName string `json:"database_name"`
	ProjectName  string `json:"project_name"`
	DryRun       bool   `json:"dry_run"`
	BatchSize    int    `json:"batch_size"`
}

// ImportResult contains the result of an import operation
type ImportResult struct {
	ProjectID    string        `json:"project_id"`
	BranchID     string        `json:"branch_id"`
	ImportedDocs int64         `json:"imported_documents"`
	WALEntries   int64         `json:"wal_entries_created"`
	Collections  []string      `json:"imported_collections"`
	Duration     time.Duration `json:"duration"`
	StartLSN     int64         `json:"start_lsn"`
	EndLSN       int64         `json:"end_lsn"`
}

// importCmd represents the import command
var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import existing MongoDB databases into Argon",
	Long: `Import existing MongoDB databases into Argon's WAL system.

This allows you to bring existing MongoDB data into Argon to enable
time travel, branching, and other Argon features on your existing data.

Available subcommands:
  preview  - Preview what would be imported
  database - Import an existing MongoDB database
  status   - Check status of import operations`,
}

// importPreviewCmd previews what would be imported
var importPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview import of an existing MongoDB database",
	Long: `Preview what would be imported from an existing MongoDB database.

This command analyzes the source database and shows:
- Collections that would be imported
- Document counts and estimated sizes
- Estimated WAL entries that would be created

Example:
  argon import preview --uri "mongodb://localhost:27017" --database "myapp"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		// Get flags
		mongoURI, _ := cmd.Flags().GetString("uri")
		databaseName, _ := cmd.Flags().GetString("database")
		outputFormat, _ := cmd.Flags().GetString("output")

		if mongoURI == "" {
			return fmt.Errorf("--uri flag is required")
		}
		if databaseName == "" {
			return fmt.Errorf("--database flag is required")
		}

		// Initialize services
		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to initialize services: %w", err)
		}

		// Preview the import
		previewData, err := services.ImportPreview(ctx, mongoURI, databaseName)
		if err != nil {
			return fmt.Errorf("failed to preview import: %w", err)
		}

		// Convert to our CLI type
		preview := convertToImportPreview(previewData)

		// Output results
		switch outputFormat {
		case "json":
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(preview)
		default:
			return printImportPreview(preview)
		}
	},
}

// importDatabaseCmd imports an existing MongoDB database
var importDatabaseCmd = &cobra.Command{
	Use:   "database",
	Short: "Import an existing MongoDB database into Argon",
	Long: `Import an existing MongoDB database into Argon's WAL system.

This creates a new Argon project and imports all data from the source
database, enabling time travel and branching capabilities.

Example:
  argon import database --uri "mongodb://localhost:27017" --database "myapp" --project "imported-myapp"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		// Get flags
		mongoURI, _ := cmd.Flags().GetString("uri")
		databaseName, _ := cmd.Flags().GetString("database")
		projectName, _ := cmd.Flags().GetString("project")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		batchSize, _ := cmd.Flags().GetInt("batch-size")
		outputFormat, _ := cmd.Flags().GetString("output")

		if mongoURI == "" {
			return fmt.Errorf("--uri flag is required")
		}
		if databaseName == "" {
			return fmt.Errorf("--database flag is required")
		}
		if projectName == "" {
			return fmt.Errorf("--project flag is required")
		}

		// Initialize services
		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to initialize services: %w", err)
		}

		// Configure import options
		opts := ImportOptions{
			MongoURI:     mongoURI,
			DatabaseName: databaseName,
			ProjectName:  projectName,
			DryRun:       dryRun,
			BatchSize:    batchSize,
		}

		// Show confirmation unless dry run
		if !dryRun {
			fmt.Printf("⚠️  About to import database '%s' into new project '%s'\n", databaseName, projectName)
			fmt.Printf("   This will create WAL entries for all existing data.\n")
			fmt.Printf("   Continue? (y/N): ")
			
			var response string
			fmt.Scanln(&response)
			if response != "y" && response != "Y" && response != "yes" {
				fmt.Println("Import cancelled.")
				return nil
			}
		}

		// Perform the import
		fmt.Printf("🚀 Starting import of database '%s'...\n", databaseName)
		if dryRun {
			fmt.Println("   (DRY RUN - no changes will be made)")
		}

		resultData, err := services.ImportDatabase(ctx, opts.MongoURI, opts.DatabaseName, opts.ProjectName, opts.DryRun, opts.BatchSize)
		if err != nil {
			return fmt.Errorf("failed to import database: %w", err)
		}

		// Convert to our CLI type
		result := convertToImportResult(resultData)

		// Output results
		switch outputFormat {
		case "json":
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(result)
		default:
			return printImportResult(result, dryRun)
		}
	},
}

// importStatusCmd shows the status of import operations
var importStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of import operations",
	Long: `Show the status of recent import operations.

This command shows information about completed imports including:
- Projects created from imports
- Import statistics and performance
- WAL state after import

Example:
  argon import status --project "imported-myapp"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Get flags
		projectName, _ := cmd.Flags().GetString("project")

		if projectName == "" {
			return fmt.Errorf("--project flag is required")
		}

		// Initialize services
		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to initialize services: %w", err)
		}

		// Get project info
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("failed to get project '%s': %w", projectName, err)
		}

		// Get main branch
		branch, err := services.Branches.GetBranch(project.ID, "main")
		if err != nil {
			return fmt.Errorf("failed to get main branch: %w", err)
		}

		// Get time travel info for status
		info, err := services.TimeTravel.GetTimeTravelInfo(branch)
		if err != nil {
			return fmt.Errorf("failed to get time travel info: %w", err)
		}

		// Print status
		fmt.Printf("📊 Import Status for Project '%s'\n", projectName)
		fmt.Printf("   Project ID: %s\n", project.ID)
		fmt.Printf("   Branch ID: %s\n", branch.ID)
		fmt.Printf("   Created: %s\n", project.CreatedAt.Format(time.RFC3339))
		fmt.Printf("   WAL Range: LSN %d - %d\n", info.EarliestLSN, info.LatestLSN)
		fmt.Printf("   Total Entries: %d\n", info.EntryCount)
		fmt.Printf("   Status: ✅ Ready for time travel and branching\n")

		return nil
	},
}

func init() {
	rootCmd.AddCommand(importCmd)
	importCmd.AddCommand(importPreviewCmd)
	importCmd.AddCommand(importDatabaseCmd)
	importCmd.AddCommand(importStatusCmd)

	// Preview command flags
	importPreviewCmd.Flags().StringP("uri", "u", "", "MongoDB connection URI (required)")
	importPreviewCmd.Flags().StringP("database", "d", "", "Database name to preview (required)")
	importPreviewCmd.Flags().StringP("output", "o", "table", "Output format: table, json")
	_ = importPreviewCmd.MarkFlagRequired("uri")
	_ = importPreviewCmd.MarkFlagRequired("database")

	// Database import command flags
	importDatabaseCmd.Flags().StringP("uri", "u", "", "MongoDB connection URI (required)")
	importDatabaseCmd.Flags().StringP("database", "d", "", "Database name to import (required)")
	importDatabaseCmd.Flags().StringP("project", "p", "", "Argon project name to create (required)")
	importDatabaseCmd.Flags().Bool("dry-run", false, "Preview import without making changes")
	importDatabaseCmd.Flags().Int("batch-size", 1000, "Number of documents to process in each batch")
	importDatabaseCmd.Flags().StringP("output", "o", "table", "Output format: table, json")
	_ = importDatabaseCmd.MarkFlagRequired("uri")
	_ = importDatabaseCmd.MarkFlagRequired("database")
	_ = importDatabaseCmd.MarkFlagRequired("project")

	// Status command flags
	importStatusCmd.Flags().StringP("project", "p", "", "Project name to check status (required)")
	_ = importStatusCmd.MarkFlagRequired("project")
}

// printImportPreview prints the import preview in a user-friendly format
func printImportPreview(preview *ImportPreview) error {
	fmt.Printf("📋 Import Preview for Database '%s'\n\n", preview.DatabaseName)

	if len(preview.Collections) == 0 {
		fmt.Println("   No collections found to import.")
		return nil
	}

	fmt.Printf("📊 Summary:\n")
	fmt.Printf("   Collections: %d\n", len(preview.Collections))
	fmt.Printf("   Documents: %s\n", formatNumber(preview.TotalDocuments))
	fmt.Printf("   Estimated Size: %s\n", formatBytes(preview.EstimatedSize))
	fmt.Printf("   Estimated WAL Entries: %s\n", formatNumber(preview.EstimatedWALEntries))

	fmt.Printf("\n📁 Collections:\n")
	for _, coll := range preview.Collections {
		fmt.Printf("   • %s\n", coll.Name)
		fmt.Printf("     Documents: %s\n", formatNumber(coll.DocumentCount))
		fmt.Printf("     Size: %s\n", formatBytes(coll.SizeBytes))
		fmt.Printf("     Indexes: %d\n", coll.IndexCount)
		fmt.Println()
	}

	fmt.Printf("💡 Next steps:\n")
	fmt.Printf("   Run import: argon import database --uri <uri> --database %s --project <project-name>\n", preview.DatabaseName)

	return nil
}

// printImportResult prints the import result in a user-friendly format
func printImportResult(result *ImportResult, dryRun bool) error {
	if dryRun {
		fmt.Printf("✅ Dry Run Complete\n\n")
		fmt.Printf("📊 Would Import:\n")
		fmt.Printf("   Documents: %s\n", formatNumber(result.ImportedDocs))
		fmt.Printf("   Collections: %d\n", len(result.Collections))
		fmt.Printf("   Duration: %v\n", result.Duration)

		fmt.Printf("\n📁 Collections:\n")
		for _, coll := range result.Collections {
			fmt.Printf("   • %s\n", coll)
		}

		fmt.Printf("\n💡 To perform actual import, remove --dry-run flag\n")
	} else {
		fmt.Printf("✅ Import Complete\n\n")
		fmt.Printf("📊 Import Results:\n")
		fmt.Printf("   Project ID: %s\n", result.ProjectID)
		fmt.Printf("   Branch ID: %s\n", result.BranchID)
		fmt.Printf("   Documents Imported: %s\n", formatNumber(result.ImportedDocs))
		fmt.Printf("   WAL Entries Created: %s\n", formatNumber(result.WALEntries))
		fmt.Printf("   Collections: %d\n", len(result.Collections))
		fmt.Printf("   Duration: %v\n", result.Duration)
		fmt.Printf("   LSN Range: %d - %d\n", result.StartLSN, result.EndLSN)

		fmt.Printf("\n📁 Imported Collections:\n")
		for _, coll := range result.Collections {
			fmt.Printf("   • %s\n", coll)
		}

		fmt.Printf("\n🚀 Your data now has time travel capabilities!\n")
		fmt.Printf("   Create branches: argon branches create <name> -p %s\n", result.ProjectID)
		fmt.Printf("   Time travel: argon time-travel info -p %s -b main\n", result.ProjectID)
	}

	return nil
}

// formatNumber formats large numbers with commas
func formatNumber(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1000000000)
}

// formatBytes formats byte sizes in human-readable format
func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
}

// convertToImportPreview converts interface{} to ImportPreview
func convertToImportPreview(data interface{}) *ImportPreview {
	// First try direct conversion using JSON marshalling/unmarshalling
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return &ImportPreview{}
	}
	
	var result ImportPreview
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return &ImportPreview{}
	}
	
	return &result
}

// convertToImportResult converts interface{} to ImportResult
func convertToImportResult(data interface{}) *ImportResult {
	// First try direct conversion using JSON marshalling/unmarshalling
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return &ImportResult{}
	}
	
	var result ImportResult
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return &ImportResult{}
	}
	
	return &result
}

