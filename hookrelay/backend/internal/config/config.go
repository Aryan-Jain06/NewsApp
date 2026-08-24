// Package config loads process configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the full configuration surface shared by the API and worker binaries.
type Config struct {
	DatabaseURL string
	RedisURL    string

	APIAddr   string
	JWTSecret string

	// Stream / consumer-group settings.
	StreamName    string
	ConsumerGroup string
	ConsumerName  string

	// Worker pool.
	WorkerCount     int
	DeliveryTimeout time.Duration

	// Scheduler re-enqueues deliveries whose next_attempt_at has passed.
	SchedulerInterval  time.Duration
	SchedulerBatchSize int

	// Reaper reclaims stream entries abandoned by crashed workers.
	ReaperInterval  time.Duration
	ReaperMinIdle   time.Duration
	ReaperBatchSize int

	// Circuit breaker.
	BreakerThreshold int
	BreakerCooldown  time.Duration

	// DeliveryMaxAge is an absolute deadline on a delivery's life. It exists
	// because circuit-breaker skips deliberately do not consume the retry
	// budget: without a deadline, deliveries queued for an endpoint that stays
	// down would be deferred forever instead of dead-lettering.
	DeliveryMaxAge time.Duration

	// RetrySchedule is a comma-separated duration list overriding the default
	// 5s,30s,2m,10m,30m,2h,5h backoff. Compressing it lets a test environment
	// exercise the real dead-letter path in seconds instead of hours.
	RetrySchedule string

	Environment string
}

// Load reads configuration from the environment, applying defaults that make a
// local `docker compose up` work with no .env file at all.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:        env("DATABASE_URL", "postgres://hookrelay:hookrelay@localhost:5432/hookrelay?sslmode=disable"),
		RedisURL:           env("REDIS_URL", "redis://localhost:6379/0"),
		APIAddr:            env("API_ADDR", ":8080"),
		JWTSecret:          env("JWT_SECRET", "dev-only-change-me"),
		StreamName:         env("STREAM_NAME", "deliveries_stream"),
		ConsumerGroup:      env("CONSUMER_GROUP", "delivery_workers"),
		ConsumerName:       env("CONSUMER_NAME", defaultConsumerName()),
		Environment:        env("ENVIRONMENT", "development"),
		WorkerCount:        envInt("WORKER_COUNT", 8),
		DeliveryTimeout:    envDuration("DELIVERY_TIMEOUT", 10*time.Second),
		SchedulerInterval:  envDuration("SCHEDULER_INTERVAL", time.Second),
		SchedulerBatchSize: envInt("SCHEDULER_BATCH_SIZE", 500),
		ReaperInterval:     envDuration("REAPER_INTERVAL", 15*time.Second),
		ReaperMinIdle:      envDuration("REAPER_MIN_IDLE", 60*time.Second),
		ReaperBatchSize:    envInt("REAPER_BATCH_SIZE", 200),
		BreakerThreshold:   envInt("BREAKER_THRESHOLD", 20),
		BreakerCooldown:    envDuration("BREAKER_COOLDOWN", 5*time.Minute),
		RetrySchedule:      env("RETRY_SCHEDULE", ""),
		DeliveryMaxAge:     envDuration("DELIVERY_MAX_AGE", 24*time.Hour),
	}
	if cfg.WorkerCount < 1 {
		return nil, fmt.Errorf("WORKER_COUNT must be >= 1, got %d", cfg.WorkerCount)
	}
	if cfg.SchedulerInterval <= 0 {
		return nil, fmt.Errorf("SCHEDULER_INTERVAL must be > 0")
	}
	return cfg, nil
}

func defaultConsumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
