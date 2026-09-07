package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

var timeTravelCmd = &cobra.Command{
	Use:   "time-travel",
	Short: "Query historical database states",
	Long:  `Travel back in time to query your MongoDB database at any previous point.`,
}

var timeTravelInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show time travel information for a branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")

		if projectName == "" || branchName == "" {
			return fmt.Errorf("--project and --branch are required")
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return err
		}

		projectID, err := resolveProjectID(services, projectName)
		if err != nil {
			return err
		}
		branch, err := services.Branches.GetBranch(projectID, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		if err := services.SyncBranch(cmd.Context(), branch.ID); err != nil {
			return err
		}
		branch, err = services.Branches.GetBranchByID(branch.ID)
		if err != nil {
			return err
		}
		info, err := services.TimeTravel.GetTimeTravelInfo(branch)
		if err != nil {
			return fmt.Errorf("failed to get time travel info: %w", err)
		}

		if jsonOutput(cmd) {
			return writeJSON(cmd, map[string]any{"branch_id": branch.ID, "earliest_lsn": info.EarliestLSN, "latest_lsn": info.LatestLSN, "entry_count": info.EntryCount, "earliest_time": info.EarliestTime, "latest_time": info.LatestTime})
		}
		fmt.Printf("🕰️  Time Travel Info for '%s/%s':\n\n", projectName, branchName)
		fmt.Printf("   LSN Range: %d → %d\n", info.EarliestLSN, info.LatestLSN)
		fmt.Printf("   Total History: %d operations\n", info.EntryCount)

		if !info.EarliestTime.IsZero() {
			fmt.Printf("   Time Range: %s → %s\n",
				info.EarliestTime.Format("2006-01-02 15:04:05"),
				info.LatestTime.Format("2006-01-02 15:04:05"))
		}

		fmt.Println()
		fmt.Println("💡 Query any point in history:")
		fmt.Printf("   argon time-travel query --project %s --branch %s --lsn %d\n",
			projectName, branchName, info.LatestLSN)

		return nil
	},
}

var timeTravelQueryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query database state at a specific point in time",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectName, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		lsnStr, _ := cmd.Flags().GetString("lsn")
		collection, _ := cmd.Flags().GetString("collection")

		if projectName == "" || branchName == "" {
			return fmt.Errorf("--project and --branch are required")
		}

		if lsnStr == "" {
			return fmt.Errorf("--lsn is required for historical queries")
		}

		lsn, err := strconv.ParseInt(lsnStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid LSN: %w", err)
		}

		services, err := newCommandServices(cmd)
		if err != nil {
			return err
		}

		projectID, err := resolveProjectID(services, projectName)
		if err != nil {
			return err
		}
		branch, err := services.Branches.GetBranch(projectID, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		if err := services.SyncBranch(cmd.Context(), branch.ID); err != nil {
			return err
		}
		branch, err = services.Branches.GetBranchByID(branch.ID)
		if err != nil {
			return err
		}
		if collection != "" {
			state, err := services.TimeTravel.MaterializeAtLSN(branch, collection, lsn)
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return writeJSON(cmd, map[string]any{"lsn": lsn, "collection": collection, "documents": state})
			}
			fmt.Printf("Collection %s at LSN %d: %d documents\n", collection, lsn, len(state))
			for id := range state {
				fmt.Println(id)
			}
		} else {
			state, err := services.Materializer.MaterializeBranchAtLSN(branch, lsn)
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return writeJSON(cmd, map[string]any{"lsn": lsn, "collections": state})
			}
			for name, documents := range state {
				fmt.Printf("%s: %d documents\n", name, len(documents))
			}
		}

		return nil
	},
}

func init() {
	// Add flags
	timeTravelInfoCmd.Flags().StringP("project", "p", "", "Project name (required)")
	timeTravelInfoCmd.Flags().StringP("branch", "b", "main", "Branch name (default: main)")
	_ = timeTravelInfoCmd.MarkFlagRequired("project")

	timeTravelQueryCmd.Flags().StringP("project", "p", "", "Project name (required)")
	timeTravelQueryCmd.Flags().StringP("branch", "b", "main", "Branch name (default: main)")
	timeTravelQueryCmd.Flags().String("lsn", "", "LSN to query (required)")
	timeTravelQueryCmd.Flags().StringP("collection", "c", "", "Collection name")
	_ = timeTravelQueryCmd.MarkFlagRequired("project")
	_ = timeTravelQueryCmd.MarkFlagRequired("lsn")

	// Add subcommands
	timeTravelCmd.AddCommand(timeTravelInfoCmd)
	timeTravelCmd.AddCommand(timeTravelQueryCmd)

	// Add to root command
	rootCmd.AddCommand(timeTravelCmd)
}
