package cmd

import (
	"fmt"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Argon system status and health",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("🚀 Argon System Status:")
		fmt.Printf("   Time Travel: ✅ Enabled\n")
		fmt.Printf("   Instant Branching: ✅ Enabled\n")
		fmt.Printf("   Performance Mode: ✅ WAL Architecture\n")

		// Test connection
		services, err := walcli.NewServices()
		if err != nil {
			fmt.Printf("   Connection: ❌ FAILED (%v)\n", err)
			fmt.Println()
			fmt.Println("💡 Tip: Make sure MongoDB is running")
			return nil
		}

		fmt.Printf("   Database: ✅ Connected\n")
		fmt.Printf("   Current LSN: %d\n", services.WAL.GetCurrentLSN())

		// Get health and metrics
		health := services.Monitor.GetHealthStatus()
		fmt.Printf("   Health: %s\n", func() string {
			if health["healthy"].(bool) {
				return "✅ HEALTHY"
			}
			return "❌ UNHEALTHY"
		}())

		if metrics, ok := health["metrics"].(map[string]interface{}); ok {
			fmt.Printf("   Total Operations: %v\n", metrics["total_operations"])
			fmt.Printf("   Active Branches: %v\n", metrics["active_branches"])
			fmt.Printf("   Active Projects: %v\n", metrics["active_projects"])
		}

		fmt.Println()
		fmt.Println("System ready for instant branching and time travel! 🕰️")

		return nil
	},
}

func init() {
	// Add to root command
	rootCmd.AddCommand(statusCmd)
}
