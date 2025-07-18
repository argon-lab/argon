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
	Short: "Git-like MongoDB branching for ML/AI workflows",
	Long: `Argon CLI - MongoDB branching system with ML-native features.

Compatible with Neon CLI patterns for zero learning curve.
Think "Neon for MongoDB" with first-class ML/AI workflow support.

Examples:
  argon auth                           # Authenticate with Argon
  argon projects list                  # List all projects
  argon branches create --name exp-1   # Create new branch
  argon connection-string              # Get MongoDB connection string

Built by MongoDB engineers for the ML/AI community.`,
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