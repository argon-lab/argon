package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

// ImportPreview contains information about what would be imported
type ImportPreview struct {
	DatabaseName        string           `json:"database_name"`
	Collections         []CollectionInfo `json:"collections"`
	TotalDocuments      int64            `json:"total_documents"`
	EstimatedSize       int64            `json:"estimated_size_bytes"`
	EstimatedWALEntries int64            `json:"estimated_wal_entries"`
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

	// Set by --online: main at ConsistentLSN is the source at
	// ConsistentClusterTime, and main follows the source from there.
	Online                bool                `json:"online,omitempty"`
	Follows               string              `json:"follows,omitempty"`
	ConsistentLSN         int64               `json:"consistent_lsn,omitempty"`
	ConsistentClusterTime *walcli.ClusterTime `json:"consistent_cluster_time,omitempty"`
	SourceImagesEnabled   []string            `json:"source_images_enabled,omitempty"`
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
  status   - Check status of import operations
  cleanup  - Recover an unpublished import after its process stopped`,
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
		mongoURI = walcli.SourceURI(mongoURI)
		databaseName, _ := cmd.Flags().GetString("database")
		outputFormat, _ := cmd.Flags().GetString("output")

		if mongoURI == "" {
			return fmt.Errorf("--uri flag (or %s) is required", walcli.SourceURIEnv)
		}
		if databaseName == "" {
			return fmt.Errorf("--database flag is required")
		}

		// Initialize services
		services, err := newCommandServices(cmd)
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
			return writeJSON(cmd, preview)
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

There are two ways to get a consistent copy.

--online imports a database that keeps being written. The copy is followed
by the source's own change stream until the project equals the source at one
cluster time; no write made during the import is lost. The source must be a
replica set. The new project's main branch then follows that database: keep
it current with "argon source follow" (or pass --follow here), and it accepts
no other writes. Nothing is written to the source, and its collections are
left as they are unless you pass --enable-source-images.

--source-quiesced is for a database nobody writes to: stop its writers and
DDL first and keep them stopped until this command finishes. The result is a
one-time copy that does not follow the source.

Either way only ordinary documents are copied, not indexes or collection
options, and a failed import is cleaned up automatically. The connection
string is used for this command only and is never stored; it can also be
given as ARGON_SOURCE_URI instead of --uri.

Examples:
  argon import database --uri "$SOURCE_URI" --database myapp --project myapp --online
  argon import database --uri "$SOURCE_URI" --database myapp --project myapp --source-quiesced`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Get flags
		mongoURI, _ := cmd.Flags().GetString("uri")
		mongoURI = walcli.SourceURI(mongoURI)
		databaseName, _ := cmd.Flags().GetString("database")
		projectName, _ := cmd.Flags().GetString("project")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		sourceQuiesced, _ := cmd.Flags().GetBool("source-quiesced")
		online, _ := cmd.Flags().GetBool("online")
		follow, _ := cmd.Flags().GetBool("follow")
		allowInexact, _ := cmd.Flags().GetBool("allow-inexact")
		enableImages, _ := cmd.Flags().GetBool("enable-source-images")
		batchSize, _ := cmd.Flags().GetInt("batch-size")
		outputFormat, _ := cmd.Flags().GetString("output")

		if mongoURI == "" {
			return fmt.Errorf("--uri flag (or %s) is required", walcli.SourceURIEnv)
		}
		if databaseName == "" {
			return fmt.Errorf("--database flag is required")
		}
		if projectName == "" {
			return fmt.Errorf("--project flag is required")
		}
		if online && sourceQuiesced {
			return fmt.Errorf("choose one of --online and --source-quiesced")
		}
		if (follow || enableImages || allowInexact) && !online {
			return fmt.Errorf("--follow, --allow-inexact and --enable-source-images apply to --online imports")
		}
		if allowInexact && !follow {
			return fmt.Errorf("--allow-inexact applies to following; pass it with --follow or to argon source follow")
		}
		if !dryRun && !sourceQuiesced && !online {
			return fmt.Errorf("pass --online to import a database that is being written (replica set required), or pause its writes and DDL and pass --source-quiesced; --yes only confirms creating the target project")
		}

		// Initialize services
		services, err := newCommandServices(cmd)
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

		// Show confirmation unless dry run or --yes.
		assumeYes, _ := cmd.Flags().GetBool("yes")
		if !dryRun && !assumeYes {
			if stat, statErr := os.Stdin.Stat(); statErr == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
				// Non-interactive (container, CI, pipe): a silent zero-exit
				// cancel here has bitten people; demand an explicit --yes.
				return fmt.Errorf("stdin is not a terminal; pass --yes to confirm the import")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  About to import database '%s' into new project '%s'\n", databaseName, projectName)
			fmt.Fprintf(cmd.ErrOrStderr(), "   This will create WAL entries for all existing data.\n")
			if enableImages {
				fmt.Fprintf(cmd.ErrOrStderr(), "   --enable-source-images changes the SOURCE: every collection of '%s' will record\n", databaseName)
				fmt.Fprintf(cmd.ErrOrStderr(), "   a pre-image of each updated or deleted document from now on.\n")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "   Continue? (y/N): ")

			var response string
			_, _ = fmt.Scanln(&response)
			if response != "y" && response != "Y" && response != "yes" {
				return fmt.Errorf("import cancelled")
			}
		}

		// Perform the import
		fmt.Fprintf(cmd.ErrOrStderr(), "🚀 Starting import of database '%s'...\n", databaseName)
		if dryRun {
			fmt.Fprintln(cmd.ErrOrStderr(), "   (DRY RUN - no changes will be made)")
		}

		var resultData interface{}
		if online && !dryRun {
			resultData, err = services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
				URI: opts.MongoURI, Database: opts.DatabaseName, Project: opts.ProjectName,
				BatchSize: opts.BatchSize, EnableSourceImages: enableImages,
			})
		} else {
			resultData, err = services.ImportDatabase(ctx, opts.MongoURI, opts.DatabaseName, opts.ProjectName, opts.DryRun, opts.BatchSize, sourceQuiesced)
		}
		if err != nil {
			return fmt.Errorf("failed to import database: %w", err)
		}

		// Convert to our CLI type
		result := convertToImportResult(resultData)

		// Output results
		switch outputFormat {
		case "json":
			err = writeJSON(cmd, result)
		default:
			err = printImportResult(result, dryRun)
		}
		if err != nil || !follow || dryRun {
			return err
		}
		// Following in the same process leaves no gap between the import's
		// consistent point and the first followed change.
		fmt.Fprintf(cmd.ErrOrStderr(), "Following %s; press Ctrl-C to stop (resume with: argon source follow -p %s)\n", databaseName, projectName)
		return services.FollowSource(ctx, projectName, walcli.FollowSourceOptions{URI: mongoURI, AllowInexact: allowInexact})
	},
}

var importCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove an unfinished import after its process has stopped",
	Long: `Recover a project name left reserved by a killed or crashed import.
Stop the original importer before running this command with --yes.
Only unpublished import data is removed; completed projects are refused.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		confirmed, _ := cmd.Flags().GetBool("yes")
		if !confirmed {
			return fmt.Errorf("stop the original importer, then pass --yes to remove its unfinished data")
		}
		name, _ := cmd.Flags().GetString("project")
		services, err := newCommandServices(cmd)
		if err != nil {
			return err
		}
		if err := services.Projects.CleanupImport(cmd.Context(), name); err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"project": name, "cleaned": true})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Removed unfinished import %q; the name can be reused.\n", name)
		return nil
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
		services, err := newCommandServices(cmd)
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
	importCmd.AddCommand(importCleanupCmd)

	// Preview command flags
	importPreviewCmd.Flags().StringP("uri", "u", "", "Source connection URI (default $ARGON_SOURCE_URI); never stored")
	importPreviewCmd.Flags().StringP("database", "d", "", "Database name to preview (required)")
	importPreviewCmd.Flags().StringP("output", "o", "table", "Output format: table, json")
	_ = importPreviewCmd.MarkFlagRequired("database")

	// Database import command flags
	importDatabaseCmd.Flags().StringP("uri", "u", "", "Source connection URI (default $ARGON_SOURCE_URI); never stored")
	importDatabaseCmd.Flags().StringP("database", "d", "", "Database name to import (required)")
	importDatabaseCmd.Flags().StringP("project", "p", "", "Argon project name to create (required)")
	importDatabaseCmd.Flags().Bool("dry-run", false, "Preview import without making changes")
	importDatabaseCmd.Flags().Bool("source-quiesced", false, "Confirm source writers and DDL are paused until the import finishes")
	importDatabaseCmd.Flags().Bool("online", false, "Import a replica-set database that is being written; main then follows it")
	importDatabaseCmd.Flags().Bool("follow", false, "With --online: keep following the source in the foreground after the import")
	importDatabaseCmd.Flags().Bool("allow-inexact", false, "With --follow: see argon source follow --allow-inexact")
	importDatabaseCmd.Flags().Bool("enable-source-images", false, "With --online: turn on changeStreamPreAndPostImages for the SOURCE collections (changes how the source stores writes)")
	importDatabaseCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt (required when stdin is not a terminal)")
	importDatabaseCmd.Flags().Int("batch-size", 1000, "Number of documents to process in each batch")
	importDatabaseCmd.Flags().StringP("output", "o", "table", "Output format: table, json")
	_ = importDatabaseCmd.MarkFlagRequired("database")
	_ = importDatabaseCmd.MarkFlagRequired("project")

	// Status command flags
	importStatusCmd.Flags().StringP("project", "p", "", "Project name to check status (required)")
	_ = importStatusCmd.MarkFlagRequired("project")
	importCleanupCmd.Flags().StringP("project", "p", "", "Unfinished import project name")
	importCleanupCmd.Flags().Bool("yes", false, "Confirm the original importer is stopped and remove its unfinished data")
	importCleanupCmd.Flags().StringP("output", "o", "table", "Output format: table, json")
	_ = importCleanupCmd.MarkFlagRequired("project")
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
	fmt.Printf("   Replica set, still being written: argon import database --uri <uri> --database %s --project <project-name> --online\n", preview.DatabaseName)
	fmt.Printf("   Writers stopped:                  argon import database --uri <uri> --database %s --project <project-name> --source-quiesced\n", preview.DatabaseName)

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
		if result.Online && result.ConsistentClusterTime != nil {
			fmt.Printf("   Consistent: LSN %d is the source at cluster time Timestamp(%d, %d)\n",
				result.ConsistentLSN, result.ConsistentClusterTime.T, result.ConsistentClusterTime.I)
			fmt.Printf("   LSNs below %d are the copy in progress, not states the source was in.\n", result.ConsistentLSN)
			fmt.Printf("   main follows database '%s'; keep it current with: argon source follow -p <project>\n", result.Follows)
			if len(result.SourceImagesEnabled) > 0 {
				fmt.Printf("   Pre- and post-images were enabled on %d source collection(s).\n", len(result.SourceImagesEnabled))
			}
		}

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
