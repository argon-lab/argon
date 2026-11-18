package cmd

import (
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

var sourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Follow the external database a project was imported from",
	Long: `A project created with "argon import database --online" stays tied to
the database it was imported from: its main branch follows that database.

  follow   keep main current with the source (runs until interrupted)
  status   what main follows, how far it has come and how exact its history is
  detach   stop following for good; main becomes an ordinary branch

The source connection string is given on each command (--uri, or the
ARGON_SOURCE_URI environment variable) and is never stored. Argon reads the
source and, only through "argon merge apply --write-back", writes single
documents to it. It never drops, renames or rewrites anything there.`,
}

var sourceFollowCmd = &cobra.Command{
	Use:   "follow",
	Short: "Keep a project's main branch current with its source database",
	Long: `Follow reads the source database's change stream and appends every
document change to the project's main branch. It resumes from the position
saved with the last applied change, so stopping and restarting it neither
skips nor repeats a change, as long as the source's oplog still holds that
position.

Nothing is written to the source, and its collections are not altered. An
update's document is read back from the source as of the update's cluster
time, which MongoDB serves for a limited time (five minutes by default). If
following falls further behind than that, the exact document is gone and
following stops as degraded. Two things avoid that:

  --enable-source-images  turns on changeStreamPreAndPostImages for the
                          source's collections, so updates carry their own
                          documents for as long as the oplog keeps them. This
                          changes the source: MongoDB then stores a pre-image
                          of every updated or deleted document.
  --allow-inexact         lets following continue: such updates are applied
                          with the document's current state, main converges
                          on the source again, and the range in between is
                          recorded as inexact history (argon source status).

A collection dropped or renamed on the source stops following as degraded;
import the database again to follow it past that point.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		uri, _ := cmd.Flags().GetString("uri")
		allowInexact, _ := cmd.Flags().GetBool("allow-inexact")
		enableImages, _ := cmd.Flags().GetBool("enable-source-images")
		uri = walcli.SourceURI(uri)
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if uri == "" {
			return fmt.Errorf("the source connection string is required: pass --uri or set %s", walcli.SourceURIEnv)
		}
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		fmt.Fprintln(cmd.ErrOrStderr(), "Following the source (Ctrl-C to stop)...")
		if err := services.FollowSource(ctx, projectName, walcli.FollowSourceOptions{URI: uri, AllowInexact: allowInexact, EnableSourceImages: enableImages}); err != nil {
			return fmt.Errorf("follow failed: %w", err)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Stopped; source position saved.")
		return nil
	},
}

var sourceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what a project follows and how far following has come",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		info, err := services.SourceStatus(cmd.Context(), projectName)
		if err != nil {
			return err
		}
		if jsonOutput(cmd) {
			if err := writeJSON(cmd, info); err != nil {
				return err
			}
		} else {
			printSourceStatus(cmd, info)
		}
		if info.State == "degraded" {
			// Scripts must not read a gap as success.
			return fmt.Errorf("following is degraded: %s", info.Error)
		}
		return nil
	},
}

func printSourceStatus(cmd *cobra.Command, info *walcli.SourceInfo) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Project %s: %s follows database %q\n", info.Project, info.Branch, info.Database)
	fmt.Fprintf(out, "  Source:      %s (replica set %s)\n", info.Location, info.ReplicaSet)
	fmt.Fprintf(out, "  State:       %s", info.State)
	if !info.UpdatedAt.IsZero() {
		fmt.Fprintf(out, " (reported %s; not proof a follower is running now)", info.UpdatedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(out)
	if info.Error != "" {
		fmt.Fprintf(out, "  Error:       %s\n", info.Error)
	}
	fmt.Fprintf(out, "  Head:        LSN %d\n", info.HeadLSN)
	if info.Position != nil {
		fmt.Fprintf(out, "  Position:    source cluster time Timestamp(%d, %d)\n", info.Position.T, info.Position.I)
	}
	if info.ConsistentTime != nil {
		fmt.Fprintf(out, "  Consistent:  LSN %d is the source at Timestamp(%d, %d)\n", info.ConsistentLSN, info.ConsistentTime.T, info.ConsistentTime.I)
	}
	if !info.LastEventAt.IsZero() {
		fmt.Fprintf(out, "  Last change: %s (stored %d ms after it happened)\n", info.LastEventAt.Format(time.RFC3339), info.LastEventLagMS)
	}
	if info.Mode == "catch_up" {
		fmt.Fprintf(out, "  History:     CATCHING UP from LSN %d: %s. Until this ends, main is not a state the source was in.\n", info.InexactOpenFromLSN, info.InexactOpenReason)
	} else {
		fmt.Fprintln(out, "  History:     exact outside the ranges below")
	}
	for _, r := range info.InexactRanges {
		fmt.Fprintf(out, "    inexact LSN %d-%d: %s (exact again at LSN %d)\n", r.FromLSN, r.ToLSN, r.Reason, r.ToLSN)
	}
	if info.InexactRangesDropped > 0 {
		fmt.Fprintf(out, "    and %d older inexact range(s) no longer listed\n", info.InexactRangesDropped)
	}
}

var sourceDetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Stop following the source for good",
	Long: `Detach removes the tie between a project and the database it was
imported from. Main keeps its history and becomes an ordinary branch: it can
be merged into, reset and checked out again, and it no longer changes when
the source does. This cannot be undone; to follow the database again, import
it into a new project. Stop any running "argon source follow" first. The
source database itself is not touched.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		confirmed, _ := cmd.Flags().GetBool("yes")
		if projectName == "" {
			return fmt.Errorf("--project is required")
		}
		if !confirmed {
			return fmt.Errorf("detaching cannot be undone; pass --yes to stop project %q following its source", projectName)
		}
		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		if err := services.DetachSource(cmd.Context(), projectName); err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"project": projectName, "detached": true})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Project %q no longer follows its source; main is an ordinary branch.\n", projectName)
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{sourceFollowCmd, sourceStatusCmd, sourceDetachCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		sourceCmd.AddCommand(c)
	}
	sourceFollowCmd.Flags().String("uri", "", "Source connection string (default $"+walcli.SourceURIEnv+"); never stored")
	sourceFollowCmd.Flags().Bool("allow-inexact", false, "Continue past updates whose exact documents the source no longer serves, recording the range as inexact")
	sourceFollowCmd.Flags().Bool("enable-source-images", false, "Turn on changeStreamPreAndPostImages for the SOURCE collections (changes how the source stores writes)")
	sourceDetachCmd.Flags().Bool("yes", false, "Confirm that following stops for good")
	for _, c := range []*cobra.Command{sourceStatusCmd, sourceDetachCmd} {
		c.Flags().StringP("output", "o", "table", "Output format: table, json")
	}
	rootCmd.AddCommand(sourceCmd)
}
