package flashsale_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/flashsale"
)

// Every correctness property must hold for both backends, so every test runs
// against both.
var backends = []string{"postgres", "redis"}

func dialTimeout() time.Duration { return 5 * time.Second }

// newReserver skips the test when the dependency is unreachable, so `go test
// ./...` still works without docker compose up.
func newReserver(t *testing.T, backend string, perUserLimit int64) flashsale.Reserver {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout())
	defer cancel()

	switch backend {
	case "postgres":
		dsn := env("TEST_DATABASE_URL", env("DATABASE_URL",
			"postgres://flashsale:flashsale@localhost:5432/flashsale?sslmode=disable"))
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Skipf("postgres unavailable (%v); run `make up` to enable this test", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			t.Skipf("postgres unavailable (%v); run `make up` to enable this test", err)
		}
		t.Cleanup(pool.Close)
		if err := flashsale.Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return flashsale.NewPGReserver(pool, perUserLimit)

	case "redis":
		rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "localhost:6379")})
		if err := rdb.Ping(ctx).Err(); err != nil {
			rdb.Close()
			t.Skipf("redis unavailable (%v); run `make up` to enable this test", err)
		}
		t.Cleanup(func() { rdb.Close() })
		return flashsale.NewRedisReserver(rdb, perUserLimit)

	default:
		t.Fatalf("unknown backend %q", backend)
		return nil
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// skuFor derives a SKU unique to the running test so repeated runs never collide.
func skuFor(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("sku-%s-%d", name, time.Now().UnixNano())
}

// TestReserveConcurrent is the acceptance test: 1000 buyers race for 100 units
// and exactly 100 must win, not roughly 100.
func TestReserveConcurrent(t *testing.T) {
	const (
		stock   = 100
		buyers  = 1000
		perUser = 1
	)

	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			reserver := newReserver(t, backend, perUser)
			ctx := context.Background()
			sku := skuFor(t)

			if err := reserver.SeedStock(ctx, sku, stock); err != nil {
				t.Fatalf("seed stock: %v", err)
			}

			var reserved, soldOut, other atomic.Int64

			// One shared channel so the requests land as simultaneously as the scheduler allows.
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(buyers)
			for i := range buyers {
				go func() {
					defer wg.Done()
					<-start

					res, err := reserver.Reserve(ctx, flashsale.Request{
						SKU:       sku,
						UserID:    fmt.Sprintf("user-%d", i),
						RequestID: fmt.Sprintf("req-%d", i),
						Qty:       1,
					})
					if err != nil {
						t.Errorf("buyer %d: %v", i, err)
						return
					}
					switch res.Status {
					case flashsale.StatusReserved:
						reserved.Add(1)
					case flashsale.StatusSoldOut:
						soldOut.Add(1)
					default:
						other.Add(1)
						t.Errorf("buyer %d: unexpected status %s", i, res.Status)
					}
					if res.Remaining < 0 {
						t.Errorf("buyer %d: negative stock %d", i, res.Remaining)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := reserved.Load(); got != stock {
				t.Errorf("reserved = %d, want exactly %d (oversold by %d)", got, stock, got-stock)
			}
			if got := soldOut.Load(); got != buyers-stock {
				t.Errorf("sold_out = %d, want %d", got, buyers-stock)
			}
			if got := other.Load(); got != 0 {
				t.Errorf("unexpected outcomes = %d, want 0", got)
			}

			stats, err := reserver.Stats(ctx, sku)
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if stats.Remaining != 0 {
				t.Errorf("remaining stock = %d, want 0", stats.Remaining)
			}
			if stats.Reservations != stock {
				t.Errorf("reservations = %d, want %d", stats.Reservations, stock)
			}
			if stats.Buyers != stock {
				t.Errorf("distinct buyers = %d, want %d", stats.Buyers, stock)
			}
			if stats.UnitsSold != stock {
				t.Errorf("units sold = %d, want %d", stats.UnitsSold, stock)
			}
		})
	}
}

// TestReserveIdempotent checks a retried request never becomes a second unit of stock.
func TestReserveIdempotent(t *testing.T) {
	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			reserver := newReserver(t, backend, 1)
			ctx := context.Background()
			sku := skuFor(t)

			if err := reserver.SeedStock(ctx, sku, 10); err != nil {
				t.Fatalf("seed stock: %v", err)
			}

			req := flashsale.Request{SKU: sku, UserID: "u1", RequestID: "same-request", Qty: 1}

			first, err := reserver.Reserve(ctx, req)
			if err != nil {
				t.Fatalf("first reserve: %v", err)
			}
			if first.Status != flashsale.StatusReserved {
				t.Fatalf("first status = %s, want reserved", first.Status)
			}

			for attempt := range 3 {
				replay, err := reserver.Reserve(ctx, req)
				if err != nil {
					t.Fatalf("replay %d: %v", attempt, err)
				}
				if replay.Status != flashsale.StatusDuplicate {
					t.Errorf("replay %d status = %s, want duplicate", attempt, replay.Status)
				}
			}

			stats, err := reserver.Stats(ctx, sku)
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if stats.Remaining != 9 {
				t.Errorf("remaining = %d, want 9 (replays must not decrement)", stats.Remaining)
			}
			if stats.Reservations != 1 {
				t.Errorf("reservations = %d, want 1", stats.Reservations)
			}
		})
	}
}

// TestReserveConcurrentReplay hammers one request_id from many goroutines: only
// one may end up holding stock.
func TestReserveConcurrentReplay(t *testing.T) {
	const attempts = 200

	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			reserver := newReserver(t, backend, 1)
			ctx := context.Background()
			sku := skuFor(t)

			if err := reserver.SeedStock(ctx, sku, 50); err != nil {
				t.Fatalf("seed stock: %v", err)
			}

			req := flashsale.Request{SKU: sku, UserID: "u1", RequestID: "one-and-only", Qty: 1}

			var reserved atomic.Int64
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(attempts)
			for i := range attempts {
				go func() {
					defer wg.Done()
					<-start
					res, err := reserver.Reserve(ctx, req)
					if err != nil {
						t.Errorf("attempt %d: %v", i, err)
						return
					}
					if res.Status == flashsale.StatusReserved {
						reserved.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := reserved.Load(); got != 1 {
				t.Errorf("reserved = %d, want exactly 1", got)
			}
			stats, err := reserver.Stats(ctx, sku)
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if stats.Remaining != 49 {
				t.Errorf("remaining = %d, want 49", stats.Remaining)
			}
		})
	}
}

// TestReservePerUserLimit checks a rejected attempt puts the stock back.
func TestReservePerUserLimit(t *testing.T) {
	const perUser = 2

	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			reserver := newReserver(t, backend, perUser)
			ctx := context.Background()
			sku := skuFor(t)

			if err := reserver.SeedStock(ctx, sku, 10); err != nil {
				t.Fatalf("seed stock: %v", err)
			}

			for attempt := range perUser {
				res, err := reserver.Reserve(ctx, flashsale.Request{
					SKU: sku, UserID: "greedy", RequestID: fmt.Sprintf("req-%d", attempt), Qty: 1,
				})
				if err != nil {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
				if res.Status != flashsale.StatusReserved {
					t.Fatalf("attempt %d status = %s, want reserved", attempt, res.Status)
				}
			}

			res, err := reserver.Reserve(ctx, flashsale.Request{
				SKU: sku, UserID: "greedy", RequestID: "one-too-many", Qty: 1,
			})
			if err != nil {
				t.Fatalf("over-limit attempt: %v", err)
			}
			if res.Status != flashsale.StatusUserLimit {
				t.Errorf("status = %s, want user_limit", res.Status)
			}

			stats, err := reserver.Stats(ctx, sku)
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if stats.Remaining != 10-perUser {
				t.Errorf("remaining = %d, want %d (rejected attempt must not consume stock)",
					stats.Remaining, 10-perUser)
			}
			if stats.UnitsSold != perUser {
				t.Errorf("units sold = %d, want %d", stats.UnitsSold, perUser)
			}
		})
	}
}

// TestReserveUnknownSKU checks an unseeded SKU is reported rather than created.
func TestReserveUnknownSKU(t *testing.T) {
	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			reserver := newReserver(t, backend, 1)
			ctx := context.Background()

			res, err := reserver.Reserve(ctx, flashsale.Request{
				SKU: skuFor(t), UserID: "u1", RequestID: "r1", Qty: 1,
			})
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if res.Status != flashsale.StatusUnknownSKU {
				t.Errorf("status = %s, want unknown_sku", res.Status)
			}

			if _, err := reserver.Stats(ctx, skuFor(t)); !errors.Is(err, flashsale.ErrUnknownSKU) {
				t.Errorf("stats err = %v, want ErrUnknownSKU", err)
			}
		})
	}
}
