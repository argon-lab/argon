package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Ephemeral agent branches with a TTL",
	Long: `A sandbox is a branch forked from a parent, checked out into its own
physical database, and stamped with a TTL. Point an agent at the
connection string; merge what you like ("argon merge preview/apply"),
undo what you don't ("argon undo"), and let the sweep reclaim whatever
is left when the TTL passes.`,
}

var sandboxCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Fork, check out and TTL-stamp a sandbox",
	Long: `Fork a branch, check it out into its own physical database and stamp it
with a TTL.

With --watch the command also captures the sandbox's writes under an
optional --actor label: capture is running before the connection string is
printed, and the command stays in the foreground until interrupted. Without
--watch, run "argon watch" for the sandbox, or create it through "argon mcp"
or "argon console", which manage capture themselves.

Collections the agent creates are captured exactly while capture is running.
Run "argon collections prepare" first only when writes to a new collection may
wait longer than MongoDB's snapshot history (five minutes by default) before
they are captured, or when one transaction changes a document more than once
right after creating its collection.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		from, _ := cmd.Flags().GetString("from")
		name, _ := cmd.Flags().GetString("name")
		ttl, _ := cmd.Flags().GetDuration("ttl")
		watch, _ := cmd.Flags().GetBool("watch")
		actor, _ := cmd.Flags().GetString("actor")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if actor != "" && !watch {
			return fmt.Errorf("--actor labels captured writes and needs --watch")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		parentID, err := resolveBranch(services, projectName, from)
		if err != nil {
			return err
		}

		if err := services.SyncBranch(cmd.Context(), parentID); err != nil {
			return err
		}
		info, err := services.Sandbox.Create(context.Background(), project.ID, parentID, name, ttl)
		if err != nil {
			return fmt.Errorf("sandbox creation failed: %w", err)
		}

		// Capture must be running before anyone holds the connection string.
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if watch {
			startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := services.StartCapture(startCtx, info.BranchID, actor)
			cancel()
			if err != nil {
				return fmt.Errorf("sandbox %q was created but capture did not start (run \"argon watch -p %s -b %s\" before writing to it): %w", info.BranchName, projectName, info.BranchName, err)
			}
		}

		if jsonOutput(cmd) {
			if err := writeJSON(cmd, map[string]any{"branch_id": info.BranchID, "name": info.BranchName, "physical_db": info.PhysicalDB, "connection_string": services.BranchConnectionString(info.PhysicalDB), "expires_at": info.ExpiresAt, "fork_lsn": info.ForkLSN, "capture_managed": watch}); err != nil {
				return err
			}
		} else {
			fmt.Printf("Sandbox %q forked from %s at LSN %d.\n", info.BranchName, info.ForkedFrom, info.ForkLSN)
			fmt.Printf("Expires: %s\n", info.ExpiresAt.Format(time.RFC3339))
			fmt.Printf("Connection string:\n  %s\n", services.BranchConnectionString(info.PhysicalDB))
			if !watch {
				fmt.Printf("Capture its writes with: argon watch -p %s -b %s\n", projectName, info.BranchName)
			}
		}
		if !watch {
			return nil
		}

		// Progress goes to stderr so --output json stays one JSON value.
		fmt.Fprintln(cmd.ErrOrStderr(), "Capturing writes (Ctrl-C to stop)...")
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := services.Ingest.Stop(stopCtx, info.BranchID); err != nil {
			return fmt.Errorf("stopping capture: %w", err)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Stopped; stream position saved.")
		return nil
	},
}

var sandboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List a project's sandboxes",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		sandboxes, err := services.Sandbox.ListSandboxes(context.Background(), project.ID)
		if err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"sandboxes": sandboxes})
		}
		if len(sandboxes) == 0 {
			fmt.Println("No sandboxes.")
			return nil
		}
		now := time.Now()
		fmt.Printf("%-20s %-10s %-25s %s\n", "SANDBOX", "STATE", "EXPIRES", "PHYSICAL DB")
		for _, b := range sandboxes {
			state := b.State
			if state == "" {
				state = "released"
			}
			expiry := b.ExpiresAt.Format(time.RFC3339)
			if b.IsExpired(now) {
				expiry += " (expired)"
			}
			fmt.Printf("%-20s %-10s %-25s %s\n", b.Name, state, expiry, b.PhysicalDB)
		}
		return nil
	},
}

var sandboxDiscardCmd = &cobra.Command{
	Use:   "discard",
	Short: "Release and delete a sandbox immediately",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" || branchName == "" {
			return fmt.Errorf("--project and --branch are required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		if err := services.Sandbox.Discard(context.Background(), branchID); err != nil {
			return fmt.Errorf("discard failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"discarded": true, "branch_id": branchID})
		}
		fmt.Println("Discarded; storage reclaimed.")
		return nil
	},
}

var sandboxKeepCmd = &cobra.Command{
	Use:   "keep",
	Short: "Remove a sandbox's TTL, keeping it as an ordinary branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		if projectName == "" || branchName == "" {
			return fmt.Errorf("--project and --branch are required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}
		if err := services.Sandbox.Keep(context.Background(), branchID); err != nil {
			return fmt.Errorf("keep failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"kept": true, "branch_id": branchID})
		}
		fmt.Println("TTL removed; the branch is permanent now.")
		return nil
	},
}

var sandboxSweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Discard every expired sandbox in a project",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		report, err := services.Sandbox.Sweep(context.Background(), project.ID)
		if err != nil {
			return fmt.Errorf("sweep failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"reaped": report.Reaped, "skipped": report.Skipped})
		}
		for _, name := range report.Reaped {
			fmt.Printf("Reaped %s\n", name)
		}
		for _, s := range report.Skipped {
			fmt.Printf("Skipped %s\n", s)
		}
		if len(report.Reaped) == 0 && len(report.Skipped) == 0 {
			fmt.Println("Nothing expired.")
		}
		return nil
	},
}

func init() {
	sandboxCreateCmd.Flags().StringP("project", "p", "", "Project name (required)")
	sandboxCreateCmd.Flags().String("from", "", "Parent branch to fork (default: main)")
	sandboxCreateCmd.Flags().String("name", "", "Sandbox name (default: generated)")
	sandboxCreateCmd.Flags().Duration("ttl", time.Hour, "Time to live before the sweep reclaims it")
	sandboxCreateCmd.Flags().Bool("watch", false, "Capture the sandbox's writes and stay in the foreground until interrupted")
	sandboxCreateCmd.Flags().String("actor", "", "Persisted actor label for captured writes (needs --watch; default: ingest)")
	for _, c := range []*cobra.Command{sandboxListCmd, sandboxSweepCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
	}
	for _, c := range []*cobra.Command{sandboxDiscardCmd, sandboxKeepCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		c.Flags().StringP("branch", "b", "", "Sandbox branch name (required)")
	}

	sandboxCmd.AddCommand(sandboxCreateCmd)
	sandboxCmd.AddCommand(sandboxListCmd)
	sandboxCmd.AddCommand(sandboxDiscardCmd)
	sandboxCmd.AddCommand(sandboxKeepCmd)
	sandboxCmd.AddCommand(sandboxSweepCmd)
	rootCmd.AddCommand(sandboxCmd)
}
