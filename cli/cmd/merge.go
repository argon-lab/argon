package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Show what merging a branch into its parent would change",
	Long: `Diff computes the three-way, document-level comparison between a
branch and its parent: the documents the merge would adopt, and the
documents both sides changed differently since the fork (conflicts).
Nothing is persisted; use "argon merge preview" to open a reviewable
merge plan.`,
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

		if err := services.SyncBranch(cmd.Context(), branchID); err != nil {
			return err
		}
		plan, err := services.Merge.Compute(branchID)
		if err != nil {
			return fmt.Errorf("diff failed: %w", err)
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"plan": plan})
		}
		fmt.Printf("Merging %s → %s\n", plan.SourceBranch, plan.TargetBranch)
		for _, c := range plan.Changes {
			action := "put   "
			if c.Delete {
				action = "delete"
			}
			fmt.Printf("  %s %s/%s\n", action, c.Collection, c.DocumentID)
		}
		for _, c := range plan.Conflicts {
			fmt.Printf("  CONFLICT %s/%s (both sides changed since the fork)\n", c.Collection, c.DocumentID)
		}
		fmt.Printf("%d change(s), %d conflict(s)\n", len(plan.Changes), len(plan.Conflicts))
		return nil
	},
}

var mergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Merge a branch into its parent through a reviewable plan",
}

var mergePreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Compute and persist a merge plan (a data pull request)",
	Long: `Compute and save a merge plan. JSON output uses canonical Extended JSON v2
for document IDs and images, preserving the saved BSON field order.
literal_dollar_paths identifies literal documents with $-prefixed keys.
sorted_images names images stored in sorted order when exact BSON was unavailable.
Write-back requires verified replacement images. Table output shows the apply
command for the target. A followed target requires --write-back,
ARGON_EXPERIMENTAL_SOURCE=1 and its source connection string (--uri or ARGON_SOURCE_URI).`,
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

		if err := services.SyncBranch(cmd.Context(), branchID); err != nil {
			return err
		}
		ctx := cmd.Context()
		plan, err := services.Merge.PreparePlan(branchID)
		if err != nil {
			return fmt.Errorf("preview failed: %w", err)
		}
		plan.ID = primitive.NewObjectID()
		var response bytes.Buffer
		if jsonOutput(cmd) {
			data, err := plan.CLIJSON()
			if err != nil {
				return err
			}
			if err := encodeJSON(&response, map[string]any{"plan": data}); err != nil {
				return err
			}
		} else {
			target, err := services.Branches.GetBranchByIDContext(ctx, plan.TargetBranchID)
			if err != nil {
				return err
			}
			fmt.Fprintf(&response, "Merging %s → %s\n", plan.SourceBranch, plan.TargetBranch)
			for _, c := range plan.Changes {
				action := "put   "
				if c.Delete {
					action = "delete"
				}
				fmt.Fprintf(&response, "  %s %s/%s\n", action, c.Collection, c.DocumentID)
			}
			for _, c := range plan.Conflicts {
				fmt.Fprintf(&response, "  CONFLICT %s/%s (both sides changed since the fork)\n", c.Collection, c.DocumentID)
			}
			fmt.Fprintf(&response, "%d change(s), %d conflict(s)\n", len(plan.Changes), len(plan.Conflicts))
			paths, err := plan.LiteralDollarPathCount()
			if err != nil {
				return err
			}
			if paths > 0 {
				fmt.Fprintf(&response, "Warning: %d image path(s) contain literal documents with $-prefixed keys. See literal_dollar_paths in JSON output.\n", paths)
			}
			fmt.Fprintf(&response, "\nPlan %s saved (pending).\n", plan.ID.Hex())
			apply := "argon merge apply " + plan.ID.Hex()
			if target.IsFollower() {
				fmt.Fprintln(&response, "Write-back requires ARGON_EXPERIMENTAL_SOURCE=1 and the source URI in ARGON_SOURCE_URI or --uri.")
				apply += " --write-back"
			}
			if len(plan.Conflicts) > 0 {
				fmt.Fprintf(&response, "Apply with: %s --strategy theirs\n", apply)
				fmt.Fprintf(&response, "To keep the target's conflicted documents: %s --strategy ours\n", apply)
			} else {
				fmt.Fprintf(&response, "Apply with: %s\n", apply)
			}
		}
		if err := services.Merge.SavePlan(ctx, plan); err != nil {
			return fmt.Errorf("preview failed for plan %s: %w", plan.ID.Hex(), err)
		}
		// A closed stdout pipe must still let us report the saved ID on stderr.
		pipe := make(chan os.Signal, 1)
		signal.Notify(pipe, syscall.SIGPIPE)
		defer signal.Stop(pipe)
		_, writeErr := response.WriteTo(cmd.OutOrStdout())
		// Include the ID even when the last error is from closing capture.
		if err := errors.Join(writeErr, closeCommandServices()); err != nil {
			return fmt.Errorf("plan %s was saved: %w", plan.ID.Hex(), err)
		}
		return nil
	},
}

var mergeApplyCmd = &cobra.Command{
	Use:   "apply <plan-id>",
	Short: "Apply a pending merge plan (--write-back is experimental)",
	Long: `Apply a pending merge plan. The --write-back option is experimental
and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable it and read
docs/OPERATIONS.md for known issues. A plan with zero writes completes
in metadata. Format-3 inserts and replacements use the saved BSON order.
Write-back refuses a replacement listed in sorted_images before claiming the plan.
Argon checks capture health in the claim transaction and waits for majority
acknowledgment. The writer has two minutes from before claim acquisition
through all source transaction and commit attempts. If a plan stays writing_back,
check the source database, let following catch up, then run "argon merge preview"
again to create a new plan.`,
	Args: func(cmd *cobra.Command, args []string) error {
		strategy, _ := cmd.Flags().GetString("strategy")
		if err := walcli.ValidateMergeStrategy(strategy); err != nil {
			return err
		}
		writeBack, _ := cmd.Flags().GetBool("write-back")
		if writeBack {
			if err := walcli.RequireExperimentalWriteBack(); err != nil {
				return err
			}
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		strategy, _ := cmd.Flags().GetString("strategy")
		writeBack, _ := cmd.Flags().GetBool("write-back")
		sourceURI, _ := cmd.Flags().GetString("uri")
		if sourceURI != "" && !writeBack {
			return fmt.Errorf("--uri names the source database for --write-back; it has no effect without it")
		}

		planID, err := primitive.ObjectIDFromHex(args[0])
		if err != nil {
			return fmt.Errorf("invalid plan ID %q", args[0])
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		plan, err := services.Merge.GetPlan(cmd.Context(), planID)
		if err != nil {
			return err
		}
		if err := plan.RequirePending(); err != nil {
			return err
		}
		if err := services.SyncBranch(cmd.Context(), plan.SourceBranchID); err != nil {
			return err
		}
		if writeBack {
			// Only this flag, given on this invocation, reaches the source
			// database. The connection string is not taken from anything
			// stored, so a plan cannot be written back by accident.
			uri := walcli.SourceURI(sourceURI)
			if uri == "" {
				return fmt.Errorf("--write-back needs the source connection string: pass --uri or set %s", walcli.SourceURIEnv)
			}
			result, err := services.WriteBackMerge(cmd.Context(), planID, strategy, uri)
			if err != nil {
				return fmt.Errorf("write-back failed: %w", err)
			}
			if jsonOutput(cmd) {
				return writeJSON(cmd, map[string]any{"plan_id": planID.Hex(), "applied": result.Applied, "conflicts_resolved": result.ConflictsResolved, "written_to_source": result.Applied > 0})
			}
			if result.Applied == 0 {
				fmt.Printf("Plan applied with zero source writes (%d conflicts resolved).\n", result.ConflictsResolved)
				return nil
			}
			fmt.Printf("Written to the source database: %d change(s) in one transaction", result.Applied)
			if result.ConflictsResolved > 0 {
				fmt.Printf(", %d conflict(s) resolved via --strategy %s", result.ConflictsResolved, strategy)
			}
			fmt.Println(".")
			fmt.Printf("%s shows them once following has read them from the source (argon source status).\n", plan.TargetBranch)
			return nil
		}
		result, err := services.Merge.Apply(context.Background(), planID, strategy)
		if err != nil {
			return fmt.Errorf("apply failed: %w", err)
		}
		if err := services.SyncBranch(cmd.Context(), plan.TargetBranchID); err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"plan_id": planID.Hex(), "applied": result.Applied, "conflicts_resolved": result.ConflictsResolved})
		}
		fmt.Printf("Merged: %d change(s) applied", result.Applied)
		if result.ConflictsResolved > 0 {
			fmt.Printf(", %d conflict(s) resolved via --strategy %s", result.ConflictsResolved, strategy)
		}
		fmt.Println(".")
		return nil
	},
}

var mergeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List a project's merge plans",
	Long: `List a project's merge plans. In JSON output, document IDs and images
use canonical Extended JSON v2. New plans show stored BSON field order,
literal_dollar_paths identifies literal documents with $-prefixed keys, and
sorted_images names images stored in sorted order when exact BSON was unavailable.
An unreadable plan is listed with its ID and error.`,
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

		entries, err := services.Merge.ListPlanEntries(cmd.Context(), project.ID)
		if err != nil {
			return err
		}
		var advice string
		for _, entry := range entries {
			if entry.Plan != nil {
				if advice = entry.Plan.WritingBackAdvice(); advice != "" {
					break
				}
			}
		}
		if jsonOutput(cmd) {
			var plans []any
			for _, entry := range entries {
				if entry.Err == nil {
					data, err := entry.Plan.CLIJSON()
					if err == nil {
						plans = append(plans, data)
						continue
					}
					entry.Err = err
				}
				plans = append(plans, map[string]string{"id": entry.ID, "error": entry.Err.Error()})
			}
			output := map[string]any{"plans": plans}
			if advice != "" {
				output["writing_back_advice"] = advice
			}
			return writeJSON(cmd, output)
		}
		if len(entries) == 0 {
			fmt.Println("No merge plans.")
			return nil
		}
		fmt.Printf("%-26s %-10s %-16s %8s %10s %s\n", "PLAN", "STATUS", "SOURCE→TARGET", "CHANGES", "CONFLICTS", "CREATED")
		for _, entry := range entries {
			if entry.Err != nil {
				fmt.Printf("%-26s error: %s\n", entry.ID, entry.Err)
				continue
			}
			p := entry.Plan
			fmt.Printf("%-26s %-10s %-16s %8d %10d %s\n",
				p.ID.Hex(), p.Status, p.SourceBranch+"→"+p.TargetBranch,
				len(p.Changes), len(p.Conflicts), p.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		if advice != "" {
			fmt.Printf("\nFor a plan in writing_back: %s\n", advice)
		}
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{diffCmd, mergePreviewCmd} {
		c.Flags().StringP("project", "p", "", "Project name (required)")
		c.Flags().StringP("branch", "b", "", "Source branch to merge into its parent (required)")
	}
	mergeListCmd.Flags().StringP("project", "p", "", "Project name (required)")
	mergeApplyCmd.Flags().String("strategy", "", "Conflict resolution: theirs (take the branch) or ours (keep the target)")
	mergeApplyCmd.Flags().Bool("write-back", false, "Experimental (requires ARGON_EXPERIMENTAL_SOURCE=1). Write the plan to the external database the target follows, in one transaction that first checks every document against the plan (requires a source bound by replica-set ID)")
	mergeApplyCmd.Flags().String("uri", "", "With --write-back: source connection string (default $ARGON_SOURCE_URI); never stored")

	mergeCmd.AddCommand(mergePreviewCmd)
	mergeCmd.AddCommand(mergeApplyCmd)
	mergeCmd.AddCommand(mergeListCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(mergeCmd)
}
