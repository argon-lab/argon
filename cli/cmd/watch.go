package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Capture a checked-out branch's direct writes into the WAL",
	Long: `Watch tails the change stream of a checked-out branch's physical
database and converts every write into WAL entries, so branching, time
travel, diff and undo keep working on data written directly through
MongoDB drivers.

Runs until interrupted. The stream position is persisted, so restarting
resumes where the previous run stopped; delivery is at-least-once and
replay is idempotent, so a crash can never lose or corrupt history.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		fmt.Println("Watching for changes (Ctrl-C to stop)...")
		if err := services.Ingest.Run(ctx, branchID); err != nil {
			return fmt.Errorf("watch failed: %w", err)
		}
		fmt.Println("Stopped; stream position saved.")
		return nil
	},
}

func init() {
	watchCmd.Flags().StringP("project", "p", "", "Project name (required)")
	watchCmd.Flags().StringP("branch", "b", "", "Branch name (default: main)")
	rootCmd.AddCommand(watchCmd)
}
