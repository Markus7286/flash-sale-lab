// Command api serves the flash-sale endpoints with every backend mounted side by
// side; the unversioned /flash-sale alias forwards to v3.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/config"
	"flash-sale/internal/flashsale"
	"flash-sale/internal/httpapi"
)

const (
	shutdownGrace = 10 * time.Second
	dialTimeout   = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	defer cancelDial()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.MaxConns = cfg.DBMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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

	handler := httpapi.NewServer(logger,
		httpapi.Backend{Version: "v1", Reserver: flashsale.NewPGReserver(pool, cfg.PerUserLimit)},
		httpapi.Backend{Version: "v2", Reserver: flashsale.NewRedisReserver(rdb, cfg.PerUserLimit, cfg.SaleKeyTTL)},
		httpapi.Backend{Version: "v3", Reserver: flashsale.NewStreamReserver(rdb, pool, cfg.PerUserLimit, cfg.SaleKeyTTL)},
	).Routes()

	srv := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.APIAddr, "per_user_limit", cfg.PerUserLimit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("api shutting down")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}
