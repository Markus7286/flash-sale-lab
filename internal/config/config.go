// Package config loads runtime settings from the environment.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

type Config struct {
	APIAddr      string
	RedisAddr    string
	DatabaseURL  string
	PerUserLimit int64
	LogLevel     slog.Level
	// MetricsAddr is the worker's own listener; the api serves /metrics from
	// APIAddr and ignores this.
	MetricsAddr string
	// DBMaxConns overrides pgxpool's default of max(4, NumCPU), which would throttle
	// the v1 baseline for reasons unrelated to the design being measured.
	DBMaxConns int32
	// SaleKeyTTL must outlast the longest sale: when it elapses the Redis SKU and
	// its idempotency and per-user keys disappear together. Zero disables expiry.
	SaleKeyTTL time.Duration

	// WorkerConsumer must be unique per worker process; a restarted container
	// reusing its old name picks its own pending entries straight back up.
	WorkerConsumer  string
	StreamBatchSize int64
	// StreamClaimIdle is how long an entry sits unacknowledged before another
	// worker assumes its consumer died and takes it over.
	StreamClaimIdle time.Duration
	// StreamMaxDeliveries bounds retries of permanent failures only; transient
	// ones such as a lost database connection are retried indefinitely.
	StreamMaxDeliveries int64
	ReconcileInterval   time.Duration
}

// Load reads the environment, falling back to values that match docker-compose.
func Load() Config {
	return Config{
		APIAddr:      env("API_ADDR", ":8080"),
		RedisAddr:    env("REDIS_ADDR", "localhost:6379"),
		DatabaseURL:  env("DATABASE_URL", ""),
		PerUserLimit: envInt("PER_USER_LIMIT", 1),
		LogLevel:     envLevel("LOG_LEVEL", slog.LevelInfo),
		MetricsAddr:  env("METRICS_ADDR", ":8081"),
		DBMaxConns:   int32(envInt("DB_MAX_CONNS", 25)),
		SaleKeyTTL:   envDuration("SALE_KEY_TTL", 24*time.Hour),

		WorkerConsumer:      env("WORKER_CONSUMER", hostname()),
		StreamBatchSize:     envInt("STREAM_BATCH_SIZE", 100),
		StreamClaimIdle:     envDuration("STREAM_CLAIM_IDLE", 30*time.Second),
		StreamMaxDeliveries: envInt("STREAM_MAX_DELIVERIES", 5),
		ReconcileInterval:   envDuration("RECONCILE_INTERVAL", 30*time.Second),
	}
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "worker"
	}
	return name
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

func envLevel(key string, fallback slog.Level) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv(key))); err != nil {
		return fallback
	}
	return level
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}
