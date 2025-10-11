package config

import (
	"os"
	"time"
)

// Config holds all application configuration
type Config struct {
	HTTPAddr    string
	GRPCAddr    string
	LogLevel    string
	DatabaseURL string
	RedisURL    string
	APIKey      string

	// Worker settings
	WorkerID       string
	WorkerQueues   string
	WorkerMaxJobs  int
	WorkerLeaseTTL time.Duration
}

// Load reads configuration from environment variables with defaults
func Load() *Config {
	return &Config{
		HTTPAddr:       getEnv("QUORRA_HTTP_ADDR", ":8080"),
		GRPCAddr:       getEnv("QUORRA_GRPC_ADDR", ":50051"),
		LogLevel:       getEnv("QUORRA_LOG_LEVEL", "info"),
		DatabaseURL:    getEnv("DATABASE_URL", "postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"),
		RedisURL:       getEnv("REDIS_URL", ""),
		APIKey:         getEnv("QUORRA_API_KEY", "dev-api-key-change-in-production"),
		WorkerID:       getEnv("QUORRA_WORKER_ID", "worker-1"),
		WorkerQueues:   getEnv("QUORRA_WORKER_QUEUES", "default"),
		WorkerMaxJobs:  getEnvInt("QUORRA_WORKER_MAX_JOBS", 5),
		WorkerLeaseTTL: getEnvDuration("QUORRA_WORKER_LEASE_TTL", 30*time.Second),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := os.Stdout.WriteString(""); err == nil {
			// Simple parse
			for _, c := range value {
				if c >= '0' && c <= '9' {
					result = result*10 + int(c-'0')
				}
			}
			if result > 0 {
				return result
			}
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}
