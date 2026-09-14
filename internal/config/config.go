// Package config loads runtime settings from the environment.
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	APIAddr      string
	RedisAddr    string
	DatabaseURL  string
	PerUserLimit int64
	// DBMaxConns overrides pgxpool's default of max(4, NumCPU), which would throttle
	// the v1 baseline for reasons unrelated to the design being measured.
	DBMaxConns int32
	// SaleKeyTTL must outlast the longest sale: when it elapses the Redis SKU and
	// its idempotency and per-user keys disappear together. Zero disables expiry.
	SaleKeyTTL time.Duration
}

// Load reads the environment, falling back to values that match docker-compose.
func Load() Config {
	return Config{
		APIAddr:      env("API_ADDR", ":8080"),
		RedisAddr:    env("REDIS_ADDR", "localhost:6379"),
		DatabaseURL:  env("DATABASE_URL", ""),
		PerUserLimit: envInt("PER_USER_LIMIT", 1),
		DBMaxConns:   int32(envInt("DB_MAX_CONNS", 25)),
		SaleKeyTTL:   envDuration("SALE_KEY_TTL", 24*time.Hour),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}
