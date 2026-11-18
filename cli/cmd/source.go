package cmd

import (
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

const experimentalSourceHelp = `Experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable
this command and read docs/OPERATIONS.md for known issues.`

var sourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Follow the external database a project was imported from (experimental)",
	Long: experimentalSourceHelp + `

A project created with "argon import database --online" stays tied to
the database it was imported from: its main branch follows that database.

  follow   keep main current with the source (runs until interrupted)
  status   what main follows, who holds its lease and how far it has come
  resync   bring main to the source's current state after following has ended
           as degraded, in place, and make it followable again
  detach   stop following for good; main becomes an ordinary branch

The source connection string is given to commands that contact the source
(--uri, or the ARGON_SOURCE_URI environment variable) and is never stored.
"argon merge apply --write-back" writes the documents in a plan.
--enable-source-images changes how the source stores writes. Both require
a replica-set ID recorded at import. Argon never drops, renames or rewrites
the source database.`,
	Args: cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := walcli.RequireExperimentalSource(); err != nil {
			return err
		}
		return rootCmd.PersistentPreRunE(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var sourceFollowCmd = &cobra.Command{
	Use:   "follow",
	Short: "Keep a project's main branch current with its source database (experimental)",
	Long: experimentalSourceHelp + `

Follow reads the source database's change stream and appends every
document change to the project's main branch. It resumes from the position
saved with the last applied change, so stopping and restarting it neither
skips nor repeats a change, as long as the source's oplog still holds that
position.

Only one follower or resync can run for a project at a time. "argon source
status" shows the owner and its last heartbeat.

Following reads the source. MongoDB serves an update's document as of its
cluster time for a limited time (five minutes by default). If
following falls further behind than that, the exact document is gone and
following stops as degraded. Two things avoid that:

  --enable-source-images  turns on changeStreamPreAndPostImages for the
                          source's collections, so updates carry their own
                          documents for as long as the oplog keeps them. This
                          changes the source: MongoDB then stores a pre-image
                          of every updated or deleted document. The project
                          must have recorded the replica-set ID at import.
                          Argon verifies the saved position and opens the
                          stream first. A degraded position must recover or
                          be resynced before the collections are altered.
  --allow-inexact         lets following continue: such updates are applied
                          with the document's current state, main converges
                          on the source again, and the range in between is
                          recorded as inexact history (argon source status).

A collection dropped or renamed on the source stops following as degraded,
and so does a saved position the source's oplog no longer holds. View
creation, modification and removal are ignored. "argon source resync"
repairs both gaps in place. With --auto-resync, follow runs that
resync itself and continues. It also completes an unfinished resync at
startup. If something else moved main's head, follow stops as degraded and
requires a manual resync. If an older Argon process checked out main,
upgrade it, stop writes to the checkout, and run "argon release -b main"
for the project before resyncing. Each resync compares the whole source
with main and leaves an inexact range in main's history.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		uri, _ := cmd.Flags().GetString("uri")
		allowInexact, _ := cmd.Flags().GetBool("allow-inexact")
		enableImages, _ := cmd.Flags().GetBool("enable-source-images")
		autoResync, _ := cmd.Flags().GetBool("auto-resync")
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
		if err := services.FollowSource(ctx, projectName, walcli.FollowSourceOptions{
			URI: uri, AllowInexact: allowInexact, EnableSourceImages: enableImages, AutoResync: autoResync,
			OnResync: func(info *walcli.ResyncInfo) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Resynced main with the source (%d put, %d deleted, inexact LSN %d-%d). Resuming following...\n",
					info.Puts, info.Deletes, info.InexactFromLSN, info.ConsistentLSN)
			},
		}); err != nil {
			return fmt.Errorf("follow failed: %w", err)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Stopped; source position saved.")
		return nil
	},
}

var sourceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what a project follows and how far following has come (experimental)",
	Long: experimentalSourceHelp + `

Show the source binding, saved position and lease owner. An unfinished
resync or degraded following exits with an error. This includes a followed
main that an older Argon process checked out.`,
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
		if info.ResyncUnfinished {
			return fmt.Errorf("a resync of main has not finished. If it was interrupted, run `argon source resync -p %s` again", info.Project)
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
	fmt.Fprintf(out, "  Identity:    %s\n", info.Identity)
	if info.WriteBackUnavailableReason != "" {
		fmt.Fprintf(out, "  Write-back:  unavailable (%s)\n", info.WriteBackUnavailableReason)
	}
	fmt.Fprintf(out, "  State:       %s", info.State)
	if !info.UpdatedAt.IsZero() {
		fmt.Fprintf(out, " (reported %s; not proof a follower is running now)", info.UpdatedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(out)
	if info.Error != "" {
		fmt.Fprintf(out, "  Error:       %s\n", info.Error)
	}
	if owner := info.Owner; owner != nil {
		fmt.Fprintf(out, "  Owner:       %s on %s (PID %d, last heartbeat %s)\n",
			owner.Kind, owner.Host, owner.PID, owner.HeartbeatAt.Format(time.RFC3339))
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
	if info.ResyncUnfinished {
		fmt.Fprintf(out, "  Resync:      UNFINISHED since %s. Main is between its last consistent state and the source's current one. If the run was interrupted, `argon source resync -p %s` completes it.\n",
			info.ResyncStartedAt.Format(time.RFC3339), info.Project)
	}
	if info.Mode == "catch_up" {
		fmt.Fprintf(out, "  History:     CATCHING UP from LSN %d: %s. Until this ends, main is not a state the source was in.\n", info.InexactOpenFromLSN, info.InexactOpenReason)
	} else if info.InexactRangesDropped > 0 {
		fmt.Fprintln(out, "  History:     some inexact ranges are no longer listed")
	} else {
		fmt.Fprintln(out, "  History:     exact outside the ranges below")
	}
	for _, r := range info.InexactRanges {
		fmt.Fprintf(out, "    inexact LSN %d-%d: %s (exact again at LSN %d)\n", r.FromLSN, r.ToLSN, r.Reason, r.ToLSN)
	}
	if info.InexactRangesDropped > 0 {
		fmt.Fprintf(out, "    %d inexact range(s) omitted\n", info.InexactRangesDropped)
	}
}

var sourceResyncCmd = &cobra.Command{
	Use:   "resync",
	Short: "Bring degraded main back to the source's current state (experimental)",
	Long: experimentalSourceHelp + `

Resync repairs degraded following without importing the database again.
Typical causes are a source collection that was dropped or renamed, a
dropped source database, or a saved position the source's oplog no longer
holds.

It takes a stream position on the source, then compares the source with main
in batches: every source document that main does not already hold is stored,
and every document main holds that the source no longer has is deleted.
That includes all documents of a collection replaced by a view. A renamed
collection ends up under its new name. Resync replays the source's changes
until main equals the source at one cluster time, and prints that point.
"argon source follow" continues from there.

Nothing is written to the source. Main keeps its history: the LSNs written by
the resync are recorded as an inexact range (argon source status), because
the states in between are not states the source was in, and what happened on
the source between the last consistent point and the resync is not in main's
history at all, only its outcome is.

A resync that is interrupted, or that fails because the source dropped or
renamed another collection while it ran, leaves main marked as unfinished.
Restart it with "argon source resync" or "argon source follow --auto-resync".
Only one follower or resync can run for a project at a time.

By default resync refuses unless following is degraded or an earlier
resync is unfinished. --force permits resyncing a healthy branch once the
lease is available. If an older Argon process checked out main, upgrade it,
stop writes to the checkout, and release the checkout before resyncing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		uri, _ := cmd.Flags().GetString("uri")
		force, _ := cmd.Flags().GetBool("force")
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

		quiet := jsonOutput(cmd)
		lastReport := time.Now()
		info, err := services.ResyncSource(ctx, projectName, walcli.ResyncSourceOptions{URI: uri, Force: force,
			Progress: func(p walcli.ResyncProgress) {
				if quiet || time.Since(lastReport) < 5*time.Second {
					return
				}
				lastReport = time.Now()
				fmt.Fprintf(cmd.ErrOrStderr(), "  %s %s: %d source documents compared, %d put, %d deleted\n", p.Phase, p.Collection, p.Compared, p.Puts, p.Deletes)
			}})
		if err != nil {
			return fmt.Errorf("resync failed: %w", err)
		}
		if quiet {
			return writeJSON(cmd, info)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Project %q: main equals database %q at source cluster time Timestamp(%d, %d) (LSN %d).\n",
			info.Project, info.Database, info.ConsistentTime.T, info.ConsistentTime.I, info.ConsistentLSN)
		fmt.Fprintf(out, "  Compared:  %d source documents in %d collection(s); %d already matched\n", info.Compared, len(info.Collections), info.Unchanged)
		fmt.Fprintf(out, "  Written:   %d put, %d deleted\n", info.Puts, info.Deletes)
		if len(info.RemovedCollections) > 0 {
			fmt.Fprintf(out, "  No longer on the source, emptied on main: %v\n", info.RemovedCollections)
		}
		if info.ConsistentLSN >= info.InexactFromLSN {
			fmt.Fprintf(out, "  History:   LSN %d-%d is recorded as inexact; main is exact again at LSN %d\n", info.InexactFromLSN, info.ConsistentLSN, info.ConsistentLSN)
		}
		fmt.Fprintf(out, "Continue with: argon source follow -p %s\n", info.Project)
		return nil
	},
}

var sourceDetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Stop following the source for good (experimental)",
	Long: experimentalSourceHelp + `

Detach removes the tie between a project and the database it was
imported from. Main keeps its history and becomes an ordinary branch: it can
be merged into, reset and checked out again, and it no longer changes when
the source does. This cannot be undone; to follow the database again, import
it into a new project. (To repair a follower that ended as degraded, use
"argon source resync" instead.) Stop any running "argon source follow" first. The
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
	for _, c := range []*cobra.Command{sourceFollowCmd, sourceStatusCmd, sourceResyncCmd, sourceDetachCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		sourceCmd.AddCommand(c)
	}
	sourceFollowCmd.Flags().String("uri", "", "Source connection string (default $"+walcli.SourceURIEnv+"); never stored")
	sourceFollowCmd.Flags().Bool("allow-inexact", false, "Continue past updates whose exact documents the source no longer serves, recording the range as inexact")
	sourceFollowCmd.Flags().Bool("enable-source-images", false, "Turn on changeStreamPreAndPostImages for the SOURCE collections (requires a replica-set ID recorded at import)")
	sourceFollowCmd.Flags().Bool("auto-resync", false, "Complete an unfinished resync or repair a source stream gap, then continue following")
	sourceResyncCmd.Flags().String("uri", "", "Source connection string (default $"+walcli.SourceURIEnv+"); never stored")
	sourceResyncCmd.Flags().Bool("force", false, "Resync even though following did not end as degraded")
	sourceDetachCmd.Flags().Bool("yes", false, "Confirm that following stops for good")
	for _, c := range []*cobra.Command{sourceStatusCmd, sourceResyncCmd, sourceDetachCmd} {
		c.Flags().StringP("output", "o", "table", "Output format: table, json")
	}
	rootCmd.AddCommand(sourceCmd)
}
