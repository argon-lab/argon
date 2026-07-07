package cmd

import (
	"context"
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
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

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}

		plan, err := services.Merge.Compute(branchID)
		if err != nil {
			return fmt.Errorf("diff failed: %w", err)
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

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		branchID, err := resolveBranch(services, projectName, branchName)
		if err != nil {
			return err
		}

		plan, err := services.Merge.Preview(context.Background(), branchID)
		if err != nil {
			return fmt.Errorf("preview failed: %w", err)
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
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		strategy, _ := cmd.Flags().GetString("strategy")

		planID, err := primitive.ObjectIDFromHex(args[0])
		if err != nil {
			return fmt.Errorf("invalid plan ID %q", args[0])
		}

		services, err := walcli.NewServices()
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}

		result, err := services.Merge.Apply(context.Background(), planID, strategy)
		if err != nil {
			return fmt.Errorf("apply failed: %w", err)
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

		services, err := walcli.NewServices()
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

	mergeCmd.AddCommand(mergePreviewCmd)
	mergeCmd.AddCommand(mergeApplyCmd)
	mergeCmd.AddCommand(mergeListCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(mergeCmd)
}
