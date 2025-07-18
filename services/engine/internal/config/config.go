package config

import (
	"os"
)

type Config struct {
	Environment string
	Port        string
	MongoURI    string
	RedisURL    string
	LogLevel    string
	
	// Storage configuration
	StorageProvider string // "s3", "gcs", "azure"
	StorageBucket   string
	AWSRegion       string
	
	// Performance settings
	ChangeStreamBatchSize int
	CompressionLevel      int
	MaxConcurrentOps      int
}

func Load() *Config {
	return &Config{
		Environment:           getEnv("ENVIRONMENT", "development"),
		Port:                  getEnv("PORT", "8080"),
		MongoURI:             getEnv("MONGODB_URI", "mongodb://admin:password@localhost:27017/?replicaSet=rs0"),
		RedisURL:             getEnv("REDIS_URL", "redis://localhost:6379"),
		LogLevel:             getEnv("LOG_LEVEL", "info"),
		StorageProvider:      getEnv("STORAGE_PROVIDER", "local"),
		StorageBucket:        getEnv("STORAGE_BUCKET", "argon-dev"),
		AWSRegion:           getEnv("AWS_REGION", "us-east-1"),
		ChangeStreamBatchSize: getEnvInt("CHANGE_STREAM_BATCH_SIZE", 100),
		CompressionLevel:     getEnvInt("COMPRESSION_LEVEL", 6),
		MaxConcurrentOps:     getEnvInt("MAX_CONCURRENT_OPS", 10),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	// For simplicity, return default for now
	// In production, add proper string to int conversion
	return defaultValue
}