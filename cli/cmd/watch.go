package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Capture a checked-out branch's direct writes into the WAL",
	Long: `Watch tails the change stream of a checked-out branch's physical
database and converts supported document writes into WAL entries, so branching, time
travel, diff and undo keep working on data written directly through
MongoDB drivers.

Runs until interrupted. The stream position is persisted, so restarting
resumes its durable checkpoint while MongoDB retains the required events
and images. Transient failures retry; missing images or unsupported DDL
stop capture with an error. --actor labels this branch, not each client.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		actor, _ := cmd.Flags().GetString("actor")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
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
		if err := services.RunCapture(ctx, branchID, actor); err != nil {
			return fmt.Errorf("watch failed: %w", err)
		}
		fmt.Println("Stopped; stream position saved.")
		return nil
	},
}

func init() {
	watchCmd.Flags().StringP("project", "p", "", "Project name (required)")
	watchCmd.Flags().StringP("branch", "b", "", "Branch name (default: main)")
	watchCmd.Flags().String("actor", "", "Persisted branch actor label (default: ingest); use a new sandbox for a new run")
	rootCmd.AddCommand(watchCmd)
}
