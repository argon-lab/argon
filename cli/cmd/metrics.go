package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Count projects, branches and sandboxes, with each project's newest LSN",
	Long: `Reads the metadata database and reports, for each project, its branches,
the sandboxes and checked-out branches among them, and the newest LSN it
has allocated.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		services, err := newCommandServices(cmd)
		if err != nil {
			return err
		}
		report, err := services.ReadMetrics(cmd.Context())
		if err != nil {
			return err
		}
		if jsonOutput(cmd) {
			return writeJSON(cmd, report)
		}

		out := cmd.OutOrStdout()
		if len(report.Projects) == 0 {
			fmt.Fprintln(out, "No projects.")
			return nil
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PROJECT\tBRANCHES\tSANDBOXES\tCHECKED OUT\tCURRENT LSN")
		for _, row := range report.Projects {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n", row.Name, row.Branches, row.Sandboxes, row.CheckedOut, row.CurrentLSN)
		}
		fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t\n", report.Branches, report.Sandboxes, report.CheckedOut)
		return w.Flush()
	},
}

func init() {
	metricsCmd.Flags().StringP("output", "o", "table", "Output format: table or json")
	rootCmd.AddCommand(metricsCmd)
}
