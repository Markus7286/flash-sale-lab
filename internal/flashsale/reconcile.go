package flashsale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type ReconcileReport struct {
	SKU       string
	Total     int64
	Remaining int64
	Sold      int64
	Queued    int64
	Persisted int64
	// Busy means Redis moved while Postgres was read, so StoresBalanced cannot be judged.
	Busy bool
}

// RedisBalanced is exact: its counters come from one MULTI.
func (r ReconcileReport) RedisBalanced() bool { return r.Remaining+r.Sold == r.Total }

// StoresBalanced says every unit sold is either persisted or still queued.
func (r ReconcileReport) StoresBalanced() bool { return r.Sold == r.Persisted+r.Queued }

type redisCounters struct {
	total, remaining, sold, queued int64
}

// Reconcile reads Redis on both sides of the Postgres query, because the two
// stores share no snapshot and only an unchanged Redis makes the comparison fair.
func Reconcile(ctx context.Context, rdb redis.Cmdable, pool *pgxpool.Pool, sku string) (ReconcileReport, error) {
	before, err := readCounters(ctx, rdb, sku)
	if err != nil {
		return ReconcileReport{}, err
	}

	var persisted int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(sum(qty), 0)::bigint FROM sale_orders WHERE sku = $1`,
		sku).Scan(&persisted); err != nil {
		return ReconcileReport{}, fmt.Errorf("sum sale orders for %q: %w", sku, err)
	}

	after, err := readCounters(ctx, rdb, sku)
	if err != nil {
		return ReconcileReport{}, err
	}

	return ReconcileReport{
		SKU:       sku,
		Total:     after.total,
		Remaining: after.remaining,
		Sold:      after.sold,
		Queued:    after.queued,
		Persisted: persisted,
		Busy:      before != after,
	}, nil
}

func readCounters(ctx context.Context, rdb redis.Cmdable, sku string) (redisCounters, error) {
	pipe := rdb.TxPipeline()
	totalCmd := pipe.Get(ctx, totalKey(sku))
	stockCmd := pipe.Get(ctx, stockKey(sku))
	soldCmd := pipe.Get(ctx, soldKey(sku))
	queuedCmd := pipe.Get(ctx, queuedKey(sku))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return redisCounters{}, fmt.Errorf("read counters for %q: %w", sku, err)
	}

	var c redisCounters
	var err error
	if c.total, err = totalCmd.Int64(); err != nil {
		return redisCounters{}, counterErr(sku, "total", err)
	}
	if c.remaining, err = stockCmd.Int64(); err != nil {
		return redisCounters{}, counterErr(sku, "stock", err)
	}
	// sold and queued only appear with the first sale.
	if c.sold, err = soldCmd.Int64(); err != nil && !errors.Is(err, redis.Nil) {
		return redisCounters{}, counterErr(sku, "sold", err)
	}
	if c.queued, err = queuedCmd.Int64(); err != nil && !errors.Is(err, redis.Nil) {
		return redisCounters{}, counterErr(sku, "queued", err)
	}
	return c, nil
}

// StockLevel is the Redis-side view of one SKU. It needs no Postgres read, so a
// metrics scrape can afford to rebuild it on every collect.
type StockLevel struct {
	SKU       string
	Total     int64
	Remaining int64
	Sold      int64
	Queued    int64
}

// StockLevels reports every SKU the registry still lists, skipping any whose
// counters have already expired underneath it.
func StockLevels(ctx context.Context, rdb redis.Cmdable) ([]StockLevel, error) {
	skus, err := RegisteredSKUs(ctx, rdb)
	if err != nil {
		return nil, err
	}

	levels := make([]StockLevel, 0, len(skus))
	for _, sku := range skus {
		c, err := readCounters(ctx, rdb, sku)
		if errors.Is(err, ErrUnknownSKU) {
			continue
		}
		if err != nil {
			return nil, err
		}
		levels = append(levels, StockLevel{
			SKU:       sku,
			Total:     c.total,
			Remaining: c.remaining,
			Sold:      c.sold,
			Queued:    c.queued,
		})
	}
	return levels, nil
}

func counterErr(sku, name string, err error) error {
	if errors.Is(err, redis.Nil) {
		return ErrUnknownSKU
	}
	return fmt.Errorf("read %s for %q: %w", name, sku, err)
}

// Reconciler checks registered SKUs on a timer. A cross-store mismatch counts only
// once it survives two consecutive checks, since the worker commits to Postgres
// before it acknowledges in Redis and one check can land in between.
type Reconciler struct {
	rdb    redis.Cmdable
	pool   *pgxpool.Pool
	logger *slog.Logger

	// suspects is only touched from the goroutine calling Check or CheckSKU.
	suspects   map[string]bool
	mismatches atomic.Int64
}

func NewReconciler(rdb redis.Cmdable, pool *pgxpool.Pool, logger *slog.Logger) *Reconciler {
	return &Reconciler{rdb: rdb, pool: pool, logger: logger, suspects: make(map[string]bool)}
}

// Mismatches counts confirmed mismatches across every check so far.
func (r *Reconciler) Mismatches() int64 { return r.mismatches.Load() }

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := r.Check(ctx); err != nil {
			r.logger.Warn("reconcile failed", "err", err)
		}
	}
}

func (r *Reconciler) Check(ctx context.Context) error {
	skus, err := RegisteredSKUs(ctx, r.rdb)
	if err != nil {
		return err
	}
	var errs []error
	for _, sku := range skus {
		if err := r.CheckSKU(ctx, sku); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) CheckSKU(ctx context.Context, sku string) error {
	rep, err := Reconcile(ctx, r.rdb, r.pool, sku)
	if errors.Is(err, ErrUnknownSKU) {
		delete(r.suspects, sku)
		return nil
	}
	if err != nil {
		return err
	}

	attrs := []any{
		"sku", rep.SKU, "total", rep.Total, "remaining", rep.Remaining, "sold", rep.Sold,
		"queued", rep.Queued, "persisted", rep.Persisted, "busy", rep.Busy,
	}

	if !rep.RedisBalanced() {
		r.mismatches.Add(1)
		r.logger.Error("reconcile mismatch: remaining + sold != total", attrs...)
	}

	switch {
	case rep.Busy:
	case rep.StoresBalanced():
		delete(r.suspects, sku)
		r.logger.Info("reconciled", attrs...)
	case r.suspects[sku]:
		r.mismatches.Add(1)
		r.logger.Error("reconcile mismatch: sold != persisted + queued", attrs...)
	default:
		r.suspects[sku] = true
		r.logger.Warn("reconcile mismatch suspected, rechecking next round", attrs...)
	}
	return nil
}
