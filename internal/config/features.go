package config

import (
	"os"
	"strconv"
)

// Features represents feature flags for the application
type Features struct {
	// WAL-related features
	EnableWAL           bool // Master switch for WAL functionality
	WALForNewProjects   bool // Use WAL for new projects
	WALForNewBranches   bool // Use WAL for new branches
	WALMigrationEnabled bool // Allow migration of existing branches to WAL
}

// GetFeatures returns the current feature configuration
func GetFeatures() *Features {
	return &Features{
		EnableWAL:           getBoolEnv("ENABLE_WAL", false),
		WALForNewProjects:   getBoolEnv("WAL_NEW_PROJECTS", false),
		WALForNewBranches:   getBoolEnv("WAL_NEW_BRANCHES", false),
		WALMigrationEnabled: getBoolEnv("WAL_MIGRATION_ENABLED", false),
	}
}

// IsWALEnabled checks if WAL is enabled for the system
func IsWALEnabled() bool {
	return GetFeatures().EnableWAL
}

// ShouldUseWALForProject determines if a project should use WAL
func ShouldUseWALForProject(isNew bool, explicitWAL bool) bool {
	features := GetFeatures()
	
	if !features.EnableWAL {
		return false
	}
	
	if explicitWAL {
		return true
	}
	
	if isNew && features.WALForNewProjects {
		return true
	}
	
	return false
}

// ShouldUseWALForBranch determines if a branch should use WAL
func ShouldUseWALForBranch(projectUseWAL bool, isNew bool, explicitWAL bool) bool {
	features := GetFeatures()
	
	if !features.EnableWAL {
		return false
	}
	
	// If project uses WAL, all branches should use WAL
	if projectUseWAL {
		return true
	}
	
	if explicitWAL {
		return true
	}
	
	if isNew && features.WALForNewBranches {
		return true
	}
	
	return false
}

// getBoolEnv gets a boolean value from environment variable
func getBoolEnv(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	
	boolValue, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	
	return boolValue
}