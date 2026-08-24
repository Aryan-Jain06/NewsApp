// Command worker runs the HookRelay delivery pipeline: a pool of stream
// consumers, the retry scheduler and the reaper.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aryan-jain06/hookrelay/backend/internal/config"
	"github.com/aryan-jain06/hookrelay/backend/internal/db"
	"github.com/aryan-jain06/hookrelay/backend/internal/queue"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/workers"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// SIGTERM is what `docker compose kill -s TERM` and orchestrators send; the
	// pool drains in-flight attempts before Run returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	rdb, err := queue.Connect(ctx, cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			slog.Warn("close redis", "error", err)
		}
	}()

	q := queue.New(rdb, cfg.StreamName, cfg.ConsumerGroup)
	workerPool, err := workers.NewPool(cfg, repos.NewStore(pool), q)
	if err != nil {
		return fmt.Errorf("build delivery pool: %w", err)
	}
	return workerPool.Run(ctx)
}

// logLevel reads LOG_LEVEL, defaulting to info.
func logLevel() slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return lvl
}
