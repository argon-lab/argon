package cmd

import (
	"fmt"
	"strconv"

	"github.com/argon-lab/argon/pkg/walcli"
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

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		branch, err := services.Branches.GetBranch(projectName, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		info, err := services.TimeTravel.GetTimeTravelInfo(branch)
		if err != nil {
			return fmt.Errorf("failed to get time travel info: %w", err)
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
			projectName, branchName, info.EarliestLSN+10)

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

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		branch, err := services.Branches.GetBranch(projectName, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		fmt.Printf("🔍 Querying database state at LSN %d...\n\n", lsn)

		if collection != "" {
			// Query specific collection
			state, err := services.TimeTravel.MaterializeAtLSN(branch, collection, lsn)
			if err != nil {
				return fmt.Errorf("failed to query historical state: %w", err)
			}

			fmt.Printf("Collection '%s' had %d documents at LSN %d:\n", collection, len(state), lsn)
			for docID := range state {
				fmt.Printf("  📄 %s\n", docID)
			}
		} else {
			// Show available collections
			fmt.Println("Available collections at this point in time:")
			fmt.Println("  (Use --collection flag to see documents)")
			fmt.Println()
			fmt.Printf("Example: argon time-travel query --project %s --branch %s --lsn %d --collection users\n",
				projectName, branchName, lsn)
		}

		return nil
	},
}

func init() {
	// Add flags
	timeTravelInfoCmd.Flags().StringP("project", "p", "", "Project name (required)")
	timeTravelInfoCmd.Flags().StringP("branch", "b", "", "Branch name (required)")
	timeTravelInfoCmd.MarkFlagRequired("project")
	timeTravelInfoCmd.MarkFlagRequired("branch")

	timeTravelQueryCmd.Flags().StringP("project", "p", "", "Project name (required)")
	timeTravelQueryCmd.Flags().StringP("branch", "b", "", "Branch name (required)")
	timeTravelQueryCmd.Flags().String("lsn", "", "LSN to query (required)")
	timeTravelQueryCmd.Flags().StringP("collection", "c", "", "Collection name")
	timeTravelQueryCmd.MarkFlagRequired("project")
	timeTravelQueryCmd.MarkFlagRequired("branch")
	timeTravelQueryCmd.MarkFlagRequired("lsn")

	// Add subcommands
	timeTravelCmd.AddCommand(timeTravelInfoCmd)
	timeTravelCmd.AddCommand(timeTravelQueryCmd)

	// Add to root command
	rootCmd.AddCommand(timeTravelCmd)
}
