// Command worker persists the orders v3 publishes to Redis Streams, and
// reconciles Redis against Postgres on a timer.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/config"
	"flash-sale/internal/flashsale"
)

const dialTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	defer cancelDial()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(dialCtx); err != nil {
		return err
	}
	if err := flashsale.Migrate(dialCtx, pool); err != nil {
		return err
	}
	logger.Info("connected to postgres", "max_conns", pool.Config().MaxConns)

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer rdb.Close()
	if err := rdb.Ping(dialCtx).Err(); err != nil {
		return err
	}
	logger.Info("connected to redis", "addr", cfg.RedisAddr)

	consumer := flashsale.NewConsumer(rdb, flashsale.NewPGOrderStore(pool), flashsale.ConsumerConfig{
		Name:          cfg.WorkerConsumer,
		BatchSize:     cfg.StreamBatchSize,
		ClaimIdle:     cfg.StreamClaimIdle,
		MaxDeliveries: cfg.StreamMaxDeliveries,
	}, logger)
	reconciler := flashsale.NewReconciler(rdb, pool, logger)

	logger.Info("worker started", "consumer", cfg.WorkerConsumer, "batch_size", cfg.StreamBatchSize,
		"claim_idle", cfg.StreamClaimIdle.String(), "max_deliveries", cfg.StreamMaxDeliveries,
		"reconcile_interval", cfg.ReconcileInterval.String())

	var wg sync.WaitGroup
	wg.Go(func() { consumer.Run(ctx) })
	wg.Go(func() { reconciler.Run(ctx, cfg.ReconcileInterval) })
	wg.Wait()

	logger.Info("worker shut down", "compensations", consumer.Compensations(),
		"reconcile_mismatches", reconciler.Mismatches())
	return nil
}
