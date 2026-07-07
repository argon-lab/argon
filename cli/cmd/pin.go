package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var pinCmd = &cobra.Command{
	Use:   "pin",
	Short: "Pin branch states as named, immutable datasets",
	Long: `A pin is a named, immutable reference to a branch state — Argon's
tag. Pinned history survives garbage collection and branch resets
forever, so a pinned eval dataset materializes identically no matter
what happens to the branch afterwards. Fork branches or TTL sandboxes
from a pin to run against the pinned state.`,
}

// resolvePin loads the project and pin named by the shared flags.
func resolvePin(services *walcli.Services, projectName, pinName string) (projectID string, pinBranchID string, pinLSN int64, err error) {
	project, err := services.Projects.GetProjectByName(projectName)
	if err != nil {
		return "", "", 0, fmt.Errorf("project %q not found: %w", projectName, err)
	}
	p, err := services.Pins.Get(project.ID, pinName)
	if err != nil {
		return "", "", 0, err
	}
	return project.ID, p.BranchID, p.LSN, nil
}

var pinCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Pin a branch state under a name",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		name, _ := cmd.Flags().GetString("name")
		lsn, _ := cmd.Flags().GetInt64("lsn")
		atTime, _ := cmd.Flags().GetString("time")
		note, _ := cmd.Flags().GetString("note")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		if branchName == "" {
			branchName = "main"
		}
		branch, err := services.Branches.GetBranch(project.ID, branchName)
		if err != nil {
			return fmt.Errorf("branch %q not found: %w", branchName, err)
		}

		if atTime != "" {
			if lsn != 0 {
				return fmt.Errorf("--lsn and --time are mutually exclusive")
			}
			t, err := time.Parse(time.RFC3339, atTime)
			if err != nil {
				return fmt.Errorf("invalid --time (want RFC3339, e.g. 2026-07-07T12:00:00Z): %w", err)
			}
			lsn, err = services.TimeTravel.FindLSNAtTime(branch, t)
			if err != nil {
				return fmt.Errorf("failed to resolve time to LSN: %w", err)
			}
		}

		p, err := services.Pins.Create(project.ID, branch.ID, name, lsn, note)
		if err != nil {
			return err
		}
		fmt.Printf("Pinned %s/%s at LSN %d as %q\n", projectName, branchName, p.LSN, p.Name)
		fmt.Println("This state now survives GC and resets until the pin is deleted.")
		return nil
	},
}

var pinListCmd = &cobra.Command{
	Use:   "list",
	Short: "List a project's pins",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		pins, err := services.Pins.List(project.ID)
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			fmt.Println("No pins.")
			return nil
		}
		fmt.Printf("%-24s %-16s %-10s %-24s %s\n", "NAME", "BRANCH", "LSN", "CREATED", "NOTE")
		for _, p := range pins {
			fmt.Printf("%-24s %-16s %-10d %-24s %s\n",
				p.Name, p.BranchName, p.LSN, p.CreatedAt.Format(time.RFC3339), p.Note)
		}
		return nil
	},
}

var pinDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a pin (its history becomes reclaimable)",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		name, _ := cmd.Flags().GetString("name")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		project, err := services.Projects.GetProjectByName(projectName)
		if err != nil {
			return fmt.Errorf("project %q not found: %w", projectName, err)
		}
		if err := services.Pins.Delete(project.ID, name); err != nil {
			return err
		}
		fmt.Printf("Deleted pin %q\n", name)
		return nil
	},
}

var pinBranchCmd = &cobra.Command{
	Use:   "branch",
	Short: "Create a durable branch from a pin",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		name, _ := cmd.Flags().GetString("name")
		as, _ := cmd.Flags().GetString("as")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		projectID, branchID, lsn, err := resolvePin(services, projectName, name)
		if err != nil {
			return err
		}
		branch, err := services.Restore.CreateBranchFromPin(projectID, branchID, as, lsn)
		if err != nil {
			return err
		}
		fmt.Printf("Created branch %q from pin %q (LSN %d)\n", branch.Name, name, lsn)
		return nil
	},
}

var pinSandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Fork a TTL sandbox from a pin (reproducible eval runs)",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		name, _ := cmd.Flags().GetString("name")
		as, _ := cmd.Flags().GetString("as")
		ttl, _ := cmd.Flags().GetDuration("ttl")

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		projectID, branchID, lsn, err := resolvePin(services, projectName, name)
		if err != nil {
			return err
		}
		if as == "" {
			suffix := make([]byte, 4)
			if _, err := rand.Read(suffix); err != nil {
				return fmt.Errorf("failed to generate sandbox name: %w", err)
			}
			as = name + "-run-" + hex.EncodeToString(suffix)
		}
		branch, err := services.Restore.CreateBranchFromPin(projectID, branchID, as, lsn)
		if err != nil {
			return err
		}
		info, err := services.Sandbox.Adopt(context.Background(), branch.ID, ttl)
		if err != nil {
			return err
		}
		fmt.Printf("Sandbox %q forked from pin %q (LSN %d)\n", info.BranchName, name, lsn)
		fmt.Printf("Connect: %s\n", services.BranchConnectionString(info.PhysicalDB))
		fmt.Printf("Expires: %s\n", info.ExpiresAt.Format(time.RFC3339))
		fmt.Printf("Run \"argon watch -p %s -b %s\" to capture writes as history.\n", projectName, info.BranchName)
		return nil
	},
}

func init() {
	pinCreateCmd.Flags().StringP("project", "p", "", "Project name (required)")
	pinCreateCmd.Flags().StringP("branch", "b", "main", "Branch to pin")
	pinCreateCmd.Flags().String("name", "", "Pin name, unique per project (required)")
	pinCreateCmd.Flags().Int64("lsn", 0, "LSN to pin (default: current head)")
	pinCreateCmd.Flags().String("time", "", "Pin the state as of this RFC3339 time instead of an LSN")
	pinCreateCmd.Flags().String("note", "", "Free-form note")
	_ = pinCreateCmd.MarkFlagRequired("project")
	_ = pinCreateCmd.MarkFlagRequired("name")

	pinListCmd.Flags().StringP("project", "p", "", "Project name (required)")
	_ = pinListCmd.MarkFlagRequired("project")

	pinDeleteCmd.Flags().StringP("project", "p", "", "Project name (required)")
	pinDeleteCmd.Flags().String("name", "", "Pin name (required)")
	_ = pinDeleteCmd.MarkFlagRequired("project")
	_ = pinDeleteCmd.MarkFlagRequired("name")

	pinBranchCmd.Flags().StringP("project", "p", "", "Project name (required)")
	pinBranchCmd.Flags().String("name", "", "Pin name (required)")
	pinBranchCmd.Flags().String("as", "", "Name for the new branch (required)")
	_ = pinBranchCmd.MarkFlagRequired("project")
	_ = pinBranchCmd.MarkFlagRequired("name")
	_ = pinBranchCmd.MarkFlagRequired("as")

	pinSandboxCmd.Flags().StringP("project", "p", "", "Project name (required)")
	pinSandboxCmd.Flags().String("name", "", "Pin name (required)")
	pinSandboxCmd.Flags().String("as", "", "Sandbox branch name (default: <pin>-run-<random>)")
	pinSandboxCmd.Flags().Duration("ttl", time.Hour, "Sandbox time-to-live")
	_ = pinSandboxCmd.MarkFlagRequired("project")
	_ = pinSandboxCmd.MarkFlagRequired("name")

	pinCmd.AddCommand(pinCreateCmd, pinListCmd, pinDeleteCmd, pinBranchCmd, pinSandboxCmd)
	rootCmd.AddCommand(pinCmd)
}
