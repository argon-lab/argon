package cmd

import (
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
)

func requireSourceImagesOptIn(cmd *cobra.Command) error {
	enableImages, _ := cmd.Flags().GetBool("enable-source-images")
	if enableImages {
		return walcli.RequireExperimentalSourceImages()
	}
	return nil
}

var sourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Follow the external database a project was imported from",
	Long: `A project created with "argon import database --online" stays tied to
the database it was imported from: its main branch follows that database.

  follow   keep main current with the source (runs until interrupted)
  status   show the verified checkpoint, health reason and next action
  resync   bring main to the source's current state after following has ended
           as degraded, in place, and make it followable again
  detach   stop following for good; main becomes an ordinary branch

The source connection string is given to commands that contact the source
(--uri, or the ARGON_SOURCE_URI environment variable) and is never stored.
"argon merge apply <plan> --write-back --approve <digest>" writes the documents
in a plan and requires a replica-set ID recorded at import.
The --enable-source-images option also requires that ID and changes how the
source stores writes. Both --enable-source-images and --auto-resync are
experimental and require ARGON_EXPERIMENTAL_SOURCE=1. Operators can also enable
changeStreamPreAndPostImages directly in MongoDB. Argon never drops,
renames or rewrites the source database.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var sourceFollowCmd = &cobra.Command{
	Use:   "follow",
	Short: "Keep a project's main branch current with its source database",
	Long: `Follow reads the source database's change stream and appends every
document change to the project's main branch. It resumes from the position
saved with the last applied change, so stopping and restarting it neither
skips nor repeats a change, as long as the source's oplog still holds that
position.

Only one follower or resync can run for a project at a time. "argon source
status" shows the owner and its last heartbeat. An expired lease revokes
ownership and the follower stops with an error. Restart following to resume
from the saved position.

Following reads the source. MongoDB serves an update's document as of its
cluster time for a limited time (five minutes by default). If
following falls further behind than that, the exact document is gone and
following stops as degraded. Two things avoid that:

  --enable-source-images  is experimental and requires
                          ARGON_EXPERIMENTAL_SOURCE=1. It turns on
                          changeStreamPreAndPostImages for the source's
                          collections, so updates carry their own
                          documents for as long as the oplog keeps them. This
                          changes the source: MongoDB then stores a pre-image
                          of every updated or deleted document. The project
                          must have recorded the replica-set ID at import.
                          Argon verifies the saved position and opens the
                          stream first. A degraded position must recover or
                          be resynced before the collections are altered.
                          Before each alteration the follower checks the
                          lease. A failure lists the collections it changed.
                          You can instead enable changeStreamPreAndPostImages
                          directly in MongoDB.
  --allow-inexact         lets following continue: such updates are applied
                          with the document's current state, main converges
                          on the source again, and the range in between is
                          recorded as inexact history (argon source status).

A collection dropped or renamed on the source stops following as degraded,
and so does a saved position the source's oplog no longer holds. View
creation, modification and removal are ignored. "argon source resync"
repairs both gaps in place. The experimental --auto-resync option requires
ARGON_EXPERIMENTAL_SOURCE=1 and lets follow run that resync and continue.
It also completes an unfinished resync at startup. If something else moved
main's head, follow stops as degraded and requires a manual resync.
If an older Argon process checked out main,
upgrade it, stop writes to the checkout, and run "argon release -b main"
for the project before resyncing. Each resync compares the whole source
with main and leaves an inexact range in main's history.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if autoResync, _ := cmd.Flags().GetBool("auto-resync"); autoResync {
			if err := walcli.RequireExperimentalAutoResync(); err != nil {
				return err
			}
		}
		return requireSourceImagesOptIn(cmd)
	},
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
	Short: "Show the source checkpoint, health reason and next action",
	Long: `Show the source binding, lease owner, saved position and last consistent
point. Both table and JSON output report health, a stable reason, a message
and a next action. Use health.reason in JSON for scripts.

Exit zero means main is verified at the reported source cluster time.
A clean stop at an exact checkpoint is healthy. Reopening the stream from
that checkpoint reports restarting and also exits zero. Write-back requires
exact. An open catch-up range, a stored capture error or an expired owner
exits nonzero even if the last report says stopped. After takeover, wait for
the replacement to verify an exact checkpoint or finish a resync. Deployment
status and doctor report the same health. See the source status table in
docs/OPERATIONS.md.`,
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
		return info.HealthError()
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
		fmt.Fprintf(out, " (last report %s)", info.UpdatedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Health:      %s (%s)\n", info.Health.Kind, info.Health.Reason)
	fmt.Fprintf(out, "  Checkpoint:  %s\n", info.Health.Message)
	if info.Health.NextAction != "" {
		fmt.Fprintf(out, "  Next:        %s\n", info.Health.NextAction)
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
		fmt.Fprintf(out, "  History:     CATCHING UP from LSN %d: %s. The current head has yet to reach a verified exact checkpoint.\n", info.InexactOpenFromLSN, info.InexactOpenReason)
	} else if info.InexactRangesDropped > 0 {
		fmt.Fprintln(out, "  History:     some inexact ranges are no longer listed")
	} else if info.Health.Kind == "healthy" {
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
	Short: "Bring degraded main back to the source's current state",
	Long: `Resync repairs degraded following without importing the database again.
Typical causes are a source collection that was dropped or renamed, a
dropped source database, a saved position the source's oplog no longer
holds, or another process moving main's head.

It takes a stream position on the source, then compares the source with main
in batches. Resync records the source BSON for changed documents and for
documents whose existing images are unverified or unavailable. It deletes
documents that main still holds after their removal from the source.
That includes all documents of a collection replaced by a view. A renamed
collection ends up under its new name. Resync replays the source's changes
until main equals the source at one cluster time, and prints that point.
If another process moves main's head before replay, the resync fails and
stays unfinished.
"argon source follow" continues from there.

Nothing is written to the source. Main keeps its history: the LSNs written by
the resync are recorded as an inexact range (argon source status), because
the states in between are not states the source was in, and what happened on
the source between the last consistent point and the resync is not in main's
history at all, only its outcome is.

A resync that is interrupted, or that fails because the source dropped or
renamed another collection while it ran, leaves main marked as unfinished.
Restart it with "argon source resync" or, with ARGON_EXPERIMENTAL_SOURCE=1,
"argon source follow --auto-resync".
Only one follower or resync can run for a project at a time.

By default resync accepts degradation, an unfinished resync, a lost owner
or missing or inconsistent checkpoint evidence. If a replacement stopped
before verifying a checkpoint and a saved position is available, start
"argon source follow" to catch up.
--force permits the full comparison once the lease is available. If an
older Argon process checked out main, upgrade it,
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
	Short: "Stop following the source for good",
	Long: `Detach removes the tie between a project and the database it was
imported from. Main keeps its history and becomes an ordinary branch: it can
be merged into, reset and checked out again, and it no longer changes when
the source does. This cannot be undone; to follow the database again, import
it into a new project. To repair a degraded follower, use "argon source resync".
Detach refuses while a write-back claim is less than five minutes old. Wait for
the writer or for the follower to settle the claim. Older unsettled plans stay
writing_back. Inspect the source for each plan reported after detach.`,
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
		ids, err := services.DetachSourceWithResult(cmd.Context(), projectName)
		if err != nil {
			return err
		}
		const advice = "These plans remain writing_back. You must inspect the source for their outcome."
		if jsonOutput(cmd) {
			output := map[string]any{"project": projectName, "detached": true}
			if len(ids) != 0 {
				output["unsettled_plans"], output["warning"] = ids, advice
			}
			return writeJSON(cmd, output)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Project %q no longer follows its source; main is an ordinary branch.\n", projectName)
		if len(ids) != 0 {
			fmt.Fprintf(out, "Unsettled plans %s. %s\n", strings.Join(ids, ", "), advice)
		}
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
	sourceFollowCmd.Flags().Bool("enable-source-images", false, "Experimental (requires ARGON_EXPERIMENTAL_SOURCE=1). Turn on changeStreamPreAndPostImages for the SOURCE collections (requires a replica-set ID recorded at import)")
	sourceFollowCmd.Flags().Bool("auto-resync", false, "Experimental (requires ARGON_EXPERIMENTAL_SOURCE=1). Complete an unfinished resync or repair a source stream gap, then continue following")
	sourceResyncCmd.Flags().String("uri", "", "Source connection string (default $"+walcli.SourceURIEnv+"); never stored")
	sourceResyncCmd.Flags().Bool("force", false, "Resync even though following did not end as degraded")
	sourceDetachCmd.Flags().Bool("yes", false, "Confirm that following stops for good")
	for _, c := range []*cobra.Command{sourceStatusCmd, sourceResyncCmd, sourceDetachCmd} {
		c.Flags().StringP("output", "o", "table", "Output format: table, json")
	}
	rootCmd.AddCommand(sourceCmd)
}
