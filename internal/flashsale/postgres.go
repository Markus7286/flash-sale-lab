package flashsale

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// uniqueViolation is the SQLSTATE PostgreSQL raises for a UNIQUE conflict.
const uniqueViolation = "23505"

// Migrate is idempotent, so every binary and every test can call it at startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// PGReserver decrements with a conditional UPDATE: under READ COMMITTED the
// WHERE predicate is re-evaluated after the row lock is taken, so a lost update
// is impossible — at the cost of serialising every buyer of a SKU on one row.
type PGReserver struct {
	pool         *pgxpool.Pool
	perUserLimit int64
}

var _ Reserver = (*PGReserver)(nil)

// NewPGReserver writes perUserLimit to products.per_user_limit at seed time.
func NewPGReserver(pool *pgxpool.Pool, perUserLimit int64) *PGReserver {
	if perUserLimit < 1 {
		perUserLimit = 1
	}
	return &PGReserver{pool: pool, perUserLimit: perUserLimit}
}

func (p *PGReserver) Name() string { return "postgres-update" }

// querier is the slice of pgx both a pool and a transaction satisfy.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p *PGReserver) Reserve(ctx context.Context, req Request) (Result, error) {
	if req.Qty < 1 {
		req.Qty = 1
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin reservation: %w", err)
	}
	// Rollback is a no-op after commit, so every early return below undoes the decrement.
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency fast path; the UNIQUE (sku, request_id) index is what actually enforces it.
	var duplicate bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM orders WHERE sku = $1 AND request_id = $2)`,
		req.SKU, req.RequestID).Scan(&duplicate); err != nil {
		return Result{}, fmt.Errorf("check request_id: %w", err)
	}
	if duplicate {
		remaining, err := stockOf(ctx, tx, req.SKU)
		if err != nil {
			return Result{}, err
		}
		return Result{Status: StatusDuplicate, Remaining: remaining}, nil
	}

	// The whole of v1: one statement whose WHERE clause cannot pass for two callers at once.
	var remaining, limit int64
	err = tx.QueryRow(ctx, `
		UPDATE products
		   SET stock = stock - $2
		 WHERE sku = $1 AND stock >= $2
		RETURNING stock, per_user_limit`,
		req.SKU, req.Qty).Scan(&remaining, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		stock, err := stockOf(ctx, tx, req.SKU)
		if errors.Is(err, ErrUnknownSKU) {
			return Result{Status: StatusUnknownSKU}, nil
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Status: StatusSoldOut, Remaining: stock}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("decrement stock: %w", err)
	}

	// Checked after the decrement on purpose: the product row lock serialises this
	// SKU, so neither the replay check nor the sum can be read stale. The fast path
	// above ran before the lock, so a concurrent replay that committed while we
	// waited is only visible here and must win over the per-user limit.
	var replayed bool
	var bought int64
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM orders WHERE sku = $1 AND request_id = $2),
		       (SELECT COALESCE(SUM(qty), 0) FROM orders WHERE sku = $1 AND user_id = $3)`,
		req.SKU, req.RequestID, req.UserID).Scan(&replayed, &bought); err != nil {
		return Result{}, fmt.Errorf("read user total: %w", err)
	}
	if replayed {
		return Result{Status: StatusDuplicate, Remaining: remaining + req.Qty}, nil
	}
	if bought+req.Qty > limit {
		// The deferred rollback puts the stock back.
		return Result{Status: StatusUserLimit, Remaining: remaining + req.Qty}, nil
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO orders (request_id, sku, user_id, qty) VALUES ($1, $2, $3, $4)`,
		req.RequestID, req.SKU, req.UserID, req.Qty); err != nil {
		if isUniqueViolation(err) {
			// A concurrent replay committed since the fast path above.
			return Result{Status: StatusDuplicate, Remaining: remaining + req.Qty}, nil
		}
		return Result{}, fmt.Errorf("insert order: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit reservation: %w", err)
	}
	return Result{Status: StatusReserved, Remaining: remaining}, nil
}

// SeedStock is destructive by design: it drops every order for the SKU.
func (p *PGReserver) SeedStock(ctx context.Context, sku string, stock int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Take the row lock Reserve takes, so an in-flight reservation commits its order
	// before the DELETE below runs instead of surviving the reset.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE sku = $1 FOR UPDATE`, sku); err != nil {
		return fmt.Errorf("lock product %q: %w", sku, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM orders WHERE sku = $1`, sku); err != nil {
		return fmt.Errorf("clear orders for %q: %w", sku, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO products (sku, stock, per_user_limit) VALUES ($1, $2, $3)
		ON CONFLICT (sku) DO UPDATE
		   SET stock = EXCLUDED.stock, per_user_limit = EXCLUDED.per_user_limit`,
		sku, stock, p.perUserLimit); err != nil {
		return fmt.Errorf("seed stock for %q: %w", sku, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit seed: %w", err)
	}
	return nil
}

// Stats is one statement, so stock and the order totals come from one snapshot
// and always balance even while reservations are landing.
func (p *PGReserver) Stats(ctx context.Context, sku string) (Stats, error) {
	var stats Stats
	err := p.pool.QueryRow(ctx, `
		SELECT p.stock, count(o.id), count(DISTINCT o.user_id), COALESCE(sum(o.qty), 0)
		  FROM products p
		  LEFT JOIN orders o ON o.sku = p.sku
		 WHERE p.sku = $1
		 GROUP BY p.sku`,
		sku).Scan(&stats.Remaining, &stats.Reservations, &stats.Buyers, &stats.UnitsSold)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stats{}, ErrUnknownSKU
	}
	if err != nil {
		return Stats{}, fmt.Errorf("read stats for %q: %w", sku, err)
	}
	return stats, nil
}

func stockOf(ctx context.Context, q querier, sku string) (int64, error) {
	var stock int64
	err := q.QueryRow(ctx, `SELECT stock FROM products WHERE sku = $1`, sku).Scan(&stock)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUnknownSKU
	}
	if err != nil {
		return 0, fmt.Errorf("read stock for %q: %w", sku, err)
	}
	return stock, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
