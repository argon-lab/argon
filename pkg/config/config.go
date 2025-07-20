package config

import "os"

// IsWALEnabled returns true if WAL is enabled via environment variable
func IsWALEnabled() bool {
	return os.Getenv("ENABLE_WAL") == "true"
}

// Features represents available features
type Features struct {
	EnableWAL           bool
	WALForNewProjects   bool
	WALForNewBranches   bool
	WALMigrationEnabled bool
}

// GetFeatures returns the current feature configuration
func GetFeatures() Features {
	enabled := IsWALEnabled()
	return Features{
		EnableWAL:           enabled,
		WALForNewProjects:   enabled,
		WALForNewBranches:   enabled,
		WALMigrationEnabled: enabled,
	}
}
