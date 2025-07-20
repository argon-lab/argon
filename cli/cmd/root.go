package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile   string
	apiKey    string
	projectID string
	output    string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "argon",
	Short: "MongoDB branching with time travel - powered by WAL architecture",
	Long: `Argon CLI - MongoDB branching system with time travel capabilities.

Experience instant branching (1ms) and query any point in history.
Built with Write-Ahead Log (WAL) architecture for maximum performance.

Examples:
  argon projects create my-project        # Create project with time travel
  argon branches create feature-x -p proj # Create instant branch
  argon time-travel info -p proj -b main  # Show time travel history
  argon status                            # System health and performance
  argon metrics                           # Detailed performance metrics

The first MongoDB database with Git-like time travel.`,
	Version: "1.0.0",
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	// Global flags (identical to Neon CLI)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.argon.yaml)")
	rootCmd.PersistentFlags().StringVar(&apiKey, "api-key", "", "Argon API key for authentication")
	rootCmd.PersistentFlags().StringVar(&projectID, "project-id", "", "Argon project ID")
	rootCmd.PersistentFlags().StringVarP(&output, "output", "o", "table", "Output format (json|yaml|table)")

	// Bind flags to viper
	viper.BindPFlag("api-key", rootCmd.PersistentFlags().Lookup("api-key"))
	viper.BindPFlag("project-id", rootCmd.PersistentFlags().Lookup("project-id"))
	viper.BindPFlag("output", rootCmd.PersistentFlags().Lookup("output"))
}

// initConfig reads in config file and ENV variables.
func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		// Search config in home directory with name ".argon" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".argon")
	}

	// Environment variables
	viper.SetEnvPrefix("ARGON")
	viper.AutomaticEnv()

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}
