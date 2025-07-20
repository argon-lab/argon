package cmd

import (
	"fmt"

	"github.com/argon-lab/argon/pkg/config"
	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var walSimpleCmd = &cobra.Command{
	Use:   "wal-simple",
	Short: "Simplified WAL operations",
	Long:  `Basic WAL operations without full driver integration.`,
}

var walSimpleStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show WAL system status and health",
	RunE: func(cmd *cobra.Command, args []string) error {
		features := config.GetFeatures()

		fmt.Println("WAL System Status:")
		fmt.Printf("  Enabled: %v\n", features.EnableWAL)
		fmt.Printf("  New Projects Use WAL: %v\n", features.WALForNewProjects)
		fmt.Printf("  New Branches Use WAL: %v\n", features.WALForNewBranches)
		fmt.Printf("  Migration Enabled: %v\n", features.WALMigrationEnabled)

		if !features.EnableWAL {
			fmt.Println("\nTo enable WAL, set environment variable: ENABLE_WAL=true")
			return nil
		}

		// Test connection
		services, err := walcli.NewServices()
		if err != nil {
			fmt.Printf("  Connection: FAILED (%v)\n", err)
			return nil
		}

		fmt.Printf("  Connection: OK\n")
		fmt.Printf("  Current LSN: %d\n", services.WAL.GetCurrentLSN())
		
		// Get health and metrics
		health := services.Monitor.GetHealthStatus()
		fmt.Printf("  Health: %s\n", func() string {
			if health["healthy"].(bool) {
				return "HEALTHY"
			}
			return "UNHEALTHY"
		}())
		
		if metrics, ok := health["metrics"].(map[string]interface{}); ok {
			fmt.Printf("  Total Operations: %v\n", metrics["total_operations"])
			fmt.Printf("  Active Branches: %v\n", metrics["active_branches"])
			fmt.Printf("  Active Projects: %v\n", metrics["active_projects"])
		}

		return nil
	},
}

var walSimpleProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Basic project management",
}

var walSimpleProjectCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new WAL-enabled project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		project, err := services.Projects.CreateProject(args[0])
		if err != nil {
			return fmt.Errorf("failed to create project: %w", err)
		}

		fmt.Printf("Created WAL-enabled project '%s' (ID: %s)\n", project.Name, project.ID)
		
		// List branches to show default main branch
		branches, _ := services.Branches.ListBranches(project.ID)
		if len(branches) > 0 {
			fmt.Printf("Default branch: %s (LSN: %d)\n", branches[0].Name, branches[0].HeadLSN)
		}
		
		return nil
	},
}

var walSimpleProjectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List WAL-enabled projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !config.IsWALEnabled() {
			return fmt.Errorf("WAL is not enabled. Set ENABLE_WAL=true to use WAL features")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		projects, err := services.Projects.ListProjects()
		if err != nil {
			return fmt.Errorf("failed to list projects: %w", err)
		}

		if len(projects) == 0 {
			fmt.Println("No WAL-enabled projects found")
			return nil
		}

		fmt.Println("WAL-Enabled Projects:")
		for _, project := range projects {
			fmt.Printf("  - %s (ID: %s)\n", project.Name, project.ID)
			
			// Show branch count
			branches, _ := services.Branches.ListBranches(project.ID)
			fmt.Printf("    Branches: %d\n", len(branches))
		}

		return nil
	},
}

var walSimpleTTInfoCmd = &cobra.Command{
	Use:   "tt-info",
	Short: "Show time travel information for a branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectID, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")

		if projectID == "" || branchName == "" {
			return fmt.Errorf("project and branch are required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		branch, err := services.Branches.GetBranch(projectID, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		info, err := services.TimeTravel.GetTimeTravelInfo(branch)
		if err != nil {
			return fmt.Errorf("failed to get time travel info: %w", err)
		}

		fmt.Printf("Time Travel Info for branch '%s':\n", info.BranchName)
		fmt.Printf("  Branch ID: %s\n", info.BranchID)
		fmt.Printf("  LSN Range: %d - %d\n", info.EarliestLSN, info.LatestLSN)
		fmt.Printf("  Total Entries: %d\n", info.EntryCount)
		
		if !info.EarliestTime.IsZero() {
			fmt.Printf("  Time Range: %s to %s\n", 
				info.EarliestTime.Format("2006-01-02 15:04:05"),
				info.LatestTime.Format("2006-01-02 15:04:05"))
		}
		
		return nil
	},
}

var walSimpleRestorePreviewCmd = &cobra.Command{
	Use:   "restore-preview",
	Short: "Preview a restore operation",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectID, _ := cmd.Flags().GetString("project")
		branchName, _ := cmd.Flags().GetString("branch")
		lsnStr, _ := cmd.Flags().GetString("lsn")

		if projectID == "" || branchName == "" || lsnStr == "" {
			return fmt.Errorf("project, branch, and LSN are required")
		}

		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		branch, err := services.Branches.GetBranch(projectID, branchName)
		if err != nil {
			return fmt.Errorf("branch not found: %w", err)
		}

		// Parse LSN
		lsn := int64(0)
		if _, err := fmt.Sscanf(lsnStr, "%d", &lsn); err != nil {
			return fmt.Errorf("invalid LSN: %w", err)
		}

		preview, err := services.Restore.GetRestorePreview(branch.ID, lsn)
		if err != nil {
			return fmt.Errorf("failed to get restore preview: %w", err)
		}

		fmt.Printf("Restore Preview for branch '%s':\n", preview.BranchName)
		fmt.Printf("  Current LSN: %d\n", preview.CurrentLSN)
		fmt.Printf("  Target LSN: %d\n", preview.TargetLSN)
		fmt.Printf("  Operations to discard: %d\n", preview.OperationsToDiscard)
		
		if len(preview.AffectedCollections) > 0 {
			fmt.Println("  Affected collections:")
			for coll, count := range preview.AffectedCollections {
				fmt.Printf("    - %s: %d operations\n", coll, count)
			}
		}

		if preview.OperationsToDiscard > 0 {
			fmt.Printf("\nWARNING: This would discard %d operations!\n", preview.OperationsToDiscard)
		} else {
			fmt.Println("\nNo operations would be discarded.")
		}

		return nil
	},
}

var walSimpleMetricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Show WAL performance metrics",
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		snapshot := services.WAL.GetMetrics()
		successRates := services.WAL.GetSuccessRates()
		
		fmt.Println("WAL Performance Metrics:")
		fmt.Printf("  Operations:\n")
		fmt.Printf("    Append: %d (%.1f%% success)\n", snapshot.AppendOps, successRates["append"]*100)
		fmt.Printf("    Query: %d (%.1f%% success)\n", snapshot.QueryOps, successRates["query"]*100)
		fmt.Printf("    Materialization: %d (%.1f%% success)\n", snapshot.MaterialOps, successRates["materialization"]*100)
		fmt.Printf("    Branch: %d\n", snapshot.BranchOps)
		fmt.Printf("    Restore: %d\n", snapshot.RestoreOps)
		
		fmt.Printf("  Latencies:\n")
		fmt.Printf("    Average Append: %v\n", snapshot.AvgAppendLatency)
		fmt.Printf("    Average Query: %v\n", snapshot.AvgQueryLatency)
		fmt.Printf("    Average Materialization: %v\n", snapshot.AvgMaterialLatency)
		
		fmt.Printf("  System:\n")
		fmt.Printf("    Current LSN: %d\n", snapshot.CurrentLSN)
		fmt.Printf("    Active Branches: %d\n", snapshot.ActiveBranches)
		fmt.Printf("    Active Projects: %d\n", snapshot.ActiveProjects)
		fmt.Printf("    Last Operation: %v\n", snapshot.LastOperationTime.Format("2006-01-02 15:04:05"))

		return nil
	},
}

var walSimpleHealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Show WAL system health and alerts",
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := walcli.NewServices()
		if err != nil {
			return err
		}

		health := services.Monitor.GetHealthStatus()
		alerts := services.Monitor.GetActiveAlerts()
		
		fmt.Println("WAL System Health:")
		fmt.Printf("  Status: %s\n", func() string {
			if health["healthy"].(bool) {
				return "HEALTHY ✅"
			}
			return "UNHEALTHY ❌"
		}())
		
		fmt.Printf("  Last Check: %v\n", health["last_check"])
		fmt.Printf("  Consecutive Failures: %v\n", health["consecutive_fails"])
		fmt.Printf("  Total Health Checks: %v\n", health["health_checks"])
		
		fmt.Printf("\nAlerts:\n")
		if len(alerts) == 0 {
			fmt.Println("  No active alerts ✅")
		} else {
			for _, alert := range alerts {
				fmt.Printf("  [%s] %s: %s\n", alert.Level, alert.Title, alert.Message)
				fmt.Printf("    Triggered: %v\n", alert.Timestamp.Format("2006-01-02 15:04:05"))
			}
		}

		return nil
	},
}

var walSimpleStorageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Show WAL storage information",
	RunE: func(cmd *cobra.Command, args []string) error {
		// This would use a storage info method if implemented
		fmt.Println("WAL Storage Information:")
		fmt.Println("  [Storage metrics would be displayed here]")
		fmt.Println("  Note: Full storage metrics implementation pending")

		return nil
	},
}

func init() {
	// Add flags
	walSimpleTTInfoCmd.Flags().StringP("project", "p", "", "Project ID")
	walSimpleTTInfoCmd.Flags().StringP("branch", "b", "", "Branch name")
	
	walSimpleRestorePreviewCmd.Flags().StringP("project", "p", "", "Project ID")
	walSimpleRestorePreviewCmd.Flags().StringP("branch", "b", "", "Branch name")
	walSimpleRestorePreviewCmd.Flags().String("lsn", "", "Target LSN")

	// Add subcommands
	walSimpleProjectCmd.AddCommand(walSimpleProjectCreateCmd)
	walSimpleProjectCmd.AddCommand(walSimpleProjectListCmd)

	walSimpleCmd.AddCommand(walSimpleStatusCmd)
	walSimpleCmd.AddCommand(walSimpleProjectCmd)
	walSimpleCmd.AddCommand(walSimpleTTInfoCmd)
	walSimpleCmd.AddCommand(walSimpleRestorePreviewCmd)
	walSimpleCmd.AddCommand(walSimpleMetricsCmd)
	walSimpleCmd.AddCommand(walSimpleHealthCmd)
	walSimpleCmd.AddCommand(walSimpleStorageCmd)

	// Add to root command
	rootCmd.AddCommand(walSimpleCmd)
}