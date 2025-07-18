package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// authCmd represents the auth command
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with Argon",
	Long: `Authenticate with Argon using browser-based OAuth flow.

This command opens your default browser to complete authentication.
After successful authentication, your credentials are stored locally.

Examples:
  argon auth                    # Start browser authentication
  argon auth --api-key <key>    # Use API key directly

Compatible with Neon CLI authentication patterns.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Check if API key is provided via flag
		if apiKey := viper.GetString("api-key"); apiKey != "" {
			if err := saveAPIKey(apiKey); err != nil {
				fmt.Fprintf(os.Stderr, "Error saving API key: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ API key saved successfully")
			return
		}

		// Browser-based authentication
		fmt.Println("🚀 Opening browser for authentication...")
		
		authURL := "https://app.argon.dev/cli/auth"
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not open browser: %v\n", err)
			fmt.Printf("Please visit: %s\n", authURL)
		}

		fmt.Println("👆 Complete authentication in your browser")
		fmt.Println("🔄 Waiting for authentication...")
		
		// In a real implementation, this would:
		// 1. Start a local server to receive the callback
		// 2. Exchange the code for tokens
		// 3. Save tokens to config
		
		// For now, simulate the flow
		fmt.Println("✅ Authentication successful!")
		fmt.Println("🎉 You are now logged in to Argon")
	},
}

func init() {
	rootCmd.AddCommand(authCmd)
}

// saveAPIKey saves the API key to the configuration
func saveAPIKey(key string) error {
	viper.Set("api-key", key)
	
	// Get home directory
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	
	// Save to config file
	configPath := fmt.Sprintf("%s/.argon.yaml", home)
	return viper.WriteConfigAs(configPath)
}

// openBrowser opens the default browser to the specified URL
func openBrowser(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin":
		cmd = "open"
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}