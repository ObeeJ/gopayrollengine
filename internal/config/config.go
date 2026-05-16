package config

import (
	"log"
	"os"
)

type Config struct {
	Port               string
	DatabaseURL        string
	RedisURL           string
	AppAPIKey          string
	AppMode            string
	MockMode           bool
	MonnifyAPIKey      string
	MonnifySecretKey   string
	MonnifyBaseURL     string
	MonnifySourceWallet string
}

// Load reads all environment variables and fatally exits if any required variable is missing.
func Load() *Config {
	cfg := &Config{
		Port:                getEnv("PORT", "8080"),
		DatabaseURL:         getEnv("DATABASE_URL", "host=localhost user=postgres password=postgres dbname=payroll_db port=5432 sslmode=disable"),
		RedisURL:            getEnv("REDIS_URL", "localhost:6379"),
		AppAPIKey:           os.Getenv("APP_API_KEY"),
		AppMode:             getEnv("APP_MODE", "api"),
		MockMode:            os.Getenv("MOCK_MODE") == "true",
		MonnifyAPIKey:       os.Getenv("MONNIFY_API_KEY"),
		MonnifySecretKey:    os.Getenv("MONNIFY_SECRET_KEY"),
		MonnifyBaseURL:      getEnv("MONNIFY_BASE_URL", "https://sandbox.monnify.com"),
		MonnifySourceWallet: os.Getenv("MONNIFY_SOURCE_WALLET"),
	}

	// Validate required variables — fail fast rather than silently misbehave
	required := map[string]string{
		"APP_API_KEY": cfg.AppAPIKey,
	}
	if !cfg.MockMode {
		required["MONNIFY_API_KEY"] = cfg.MonnifyAPIKey
		required["MONNIFY_SECRET_KEY"] = cfg.MonnifySecretKey
		required["MONNIFY_SOURCE_WALLET"] = cfg.MonnifySourceWallet
	}

	for name, val := range required {
		if val == "" {
			log.Fatalf("FATAL: required environment variable %s is not set", name)
		}
	}

	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
