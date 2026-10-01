// Command worker persists the orders v3 publishes to Redis Streams, and
// reconciles Redis against Postgres on a timer.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/config"
	"flash-sale/internal/flashsale"
	"flash-sale/internal/metrics"
)

const (
	dialTimeout   = 10 * time.Second
	shutdownGrace = 10 * time.Second
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(logger, cfg); err != nil {
		logger.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, cfg config.Config) error {
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
	defer func() { _ = rdb.Close() }()
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
		"reconcile_interval", cfg.ReconcileInterval.String(), "metrics_addr", cfg.MetricsAddr)

	metrics.RegisterWorker(consumer, reconciler)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Go(func() { consumer.Run(ctx) })
	wg.Go(func() { reconciler.Run(ctx, cfg.ReconcileInterval) })
	wg.Go(func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "err", err)
			stop() // a worker nobody can observe is worse than a worker that restarts
		}
	})
	wg.Go(func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancelShutdown()
		_ = metricsSrv.Shutdown(shutdownCtx)
	})
	wg.Wait()

	logger.Info("worker shut down", "compensations", consumer.Compensations(),
		"reconcile_mismatches", reconciler.Mismatches())
	return nil
}
