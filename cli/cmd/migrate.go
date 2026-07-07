package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate-wal",
	Short: "Migrate a project's WAL from schema v1 to v2",
	Long: `Rewrites legacy schema-v1 WAL entries (which stored filter/update
expressions) into the v2 physical-log format (full post/pre-images) in
place, preserving LSNs. v2 replay refuses legacy entries because they
cannot be replayed deterministically; run this once per project after
upgrading.

The migration is idempotent: running it again finds nothing to rewrite.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}

		fmt.Printf("Migrating WAL for project %q (%s)...\n", project.Name, project.ID)
		result, err := services.Migrate.MigrateProject(context.Background(), project.ID)
		if err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}

		fmt.Printf("Done.\n")
		fmt.Printf("  Branches visited:  %d\n", result.BranchesVisited)
		fmt.Printf("  Entries rewritten: %d\n", result.EntriesRewritten)
		fmt.Printf("  Entries removed:   %d (no-op legacy operations)\n", result.EntriesRemoved)
		return nil
	},
}

func init() {
	migrateCmd.Flags().StringP("project", "p", "", "Project name to migrate (required)")
	rootCmd.AddCommand(migrateCmd)
}
