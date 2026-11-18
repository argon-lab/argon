package cmd

import (
	"context"
	"fmt"

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
		plan, err := services.Merge.Preview(context.Background(), branchID)
		if err != nil {
			return fmt.Errorf("preview failed: %w", err)
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
		fmt.Printf("\nPlan %s saved (pending).\n", plan.ID.Hex())
		if len(plan.Conflicts) > 0 {
			fmt.Printf("Apply with: argon merge apply %s --strategy theirs|ours\n", plan.ID.Hex())
		} else {
			fmt.Printf("Apply with: argon merge apply %s\n", plan.ID.Hex())
		}
		return nil
	},
}

var mergeApplyCmd = &cobra.Command{
	Use:   "apply <plan-id>",
	Short: "Apply a pending merge plan",
	Long: "Apply a pending merge plan. If a plan stays writing_back, " +
		"check the source database, let following catch up, then run \"argon merge preview\" again to create a new plan.",
	Args: cobra.ExactArgs(1),
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
				return writeJSON(cmd, map[string]any{"plan_id": planID.Hex(), "applied": result.Applied, "conflicts_resolved": result.ConflictsResolved, "written_to_source": true})
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

		plans, err := services.Merge.ListPlans(context.Background(), project.ID)
		if err != nil {
			return err
		}
		var advice string
		for _, plan := range plans {
			if advice = plan.WritingBackAdvice(); advice != "" {
				break
			}
		}
		if jsonOutput(cmd) {
			output := map[string]any{"plans": plans}
			if advice != "" {
				output["writing_back_advice"] = advice
			}
			return writeJSON(cmd, output)
		}
		if len(plans) == 0 {
			fmt.Println("No merge plans.")
			return nil
		}
		fmt.Printf("%-26s %-10s %-16s %8s %10s %s\n", "PLAN", "STATUS", "SOURCE→TARGET", "CHANGES", "CONFLICTS", "CREATED")
		for _, p := range plans {
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
	mergeApplyCmd.Flags().Bool("write-back", false, "Write the plan to the external database in one transaction after checking every document against the plan (requires a source bound by replica-set ID)")
	mergeApplyCmd.Flags().String("uri", "", "With --write-back: source connection string (default $ARGON_SOURCE_URI); never stored")

	mergeCmd.AddCommand(mergePreviewCmd)
	mergeCmd.AddCommand(mergeApplyCmd)
	mergeCmd.AddCommand(mergeListCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(mergeCmd)
}
