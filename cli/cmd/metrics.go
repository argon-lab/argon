package cmd

import (
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Show performance metrics",
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		snapshot := services.WAL.GetMetrics()
		successRates := services.WAL.GetSuccessRates()
		
		fmt.Println("📊 Argon Performance Metrics:")
		fmt.Printf("\n   Operations:\n")
		fmt.Printf("     Append: %d (%.1f%% success)\n", snapshot.AppendOps, successRates["append"]*100)
		fmt.Printf("     Query: %d (%.1f%% success)\n", snapshot.QueryOps, successRates["query"]*100)
		fmt.Printf("     Materialization: %d (%.1f%% success)\n", snapshot.MaterialOps, successRates["materialization"]*100)
		fmt.Printf("     Branch: %d\n", snapshot.BranchOps)
		fmt.Printf("     Restore: %d\n", snapshot.RestoreOps)
		
		fmt.Printf("\n   Performance:\n")
		fmt.Printf("     Average Append: %v\n", snapshot.AvgAppendLatency)
		fmt.Printf("     Average Query: %v\n", snapshot.AvgQueryLatency)
		fmt.Printf("     Average Materialization: %v\n", snapshot.AvgMaterialLatency)
		
		fmt.Printf("\n   System:\n")
		fmt.Printf("     Current LSN: %d\n", snapshot.CurrentLSN)
		fmt.Printf("     Active Branches: %d\n", snapshot.ActiveBranches)
		fmt.Printf("     Active Projects: %d\n", snapshot.ActiveProjects)
		fmt.Printf("     Last Operation: %v\n", snapshot.LastOperationTime.Format("2006-01-02 15:04:05"))

		return nil
	},
}

func init() {
	// Add to root command  
	rootCmd.AddCommand(metricsCmd)
}