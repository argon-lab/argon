package cmd

import (
	"argon-cli/internal/api"

	"github.com/spf13/viper"
)

// getAPIClient returns an API client instance
func getAPIClient() *api.Client {
	baseURL := viper.GetString("api_url")
	if baseURL == "" {
		baseURL = "http://localhost:3000" // Default API URL
	}
	return api.NewClient(baseURL)
}