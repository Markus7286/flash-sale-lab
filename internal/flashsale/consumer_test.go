package flashsale_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/flashsale"
)

type streamSale struct {
	rdb      *redis.Client
	pool     *pgxpool.Pool
	reserver *flashsale.StreamReserver
	sku      string
}

func newStreamSale(t *testing.T, stock int64) streamSale {
	t.Helper()
	s := streamSale{rdb: newRedis(t), pool: newPool(t), sku: skuFor(t)}
	s.reserver = flashsale.NewStreamReserver(s.rdb, s.pool, 1, testKeyTTL)
	if err := s.reserver.SeedStock(context.Background(), s.sku, stock); err != nil {
		t.Fatalf("seed stock: %v", err)
	}
	return s
}

func (s streamSale) key(suffix string) string { return "{" + s.sku + "}:" + suffix }

// consumer is pinned to this test's SKU, so it never settles entries that belong
// to another test or to a local bench run sharing the same Redis.
func (s streamSale) consumer(store flashsale.OrderStore, claimIdle time.Duration, maxDeliveries int64) *flashsale.Consumer {
	return flashsale.NewConsumer(s.rdb, store, flashsale.ConsumerConfig{
		Name:          "test-" + s.sku,
		BatchSize:     50,
		ClaimIdle:     claimIdle,
		MaxDeliveries: maxDeliveries,
		Block:         50 * time.Millisecond,
		SKUs:          []string{s.sku},
	}, slog.New(slog.DiscardHandler))
}

func (s streamSale) reserve(t *testing.T, requestID string) {
	t.Helper()
	res, err := s.reserver.Reserve(context.Background(), flashsale.Request{
		SKU: s.sku, UserID: "user-" + requestID, RequestID: requestID, Qty: 1,
	})
	if err != nil {
		t.Fatalf("reserve %s: %v", requestID, err)
	}
	if res.Status != flashsale.StatusReserved {
		t.Fatalf("reserve %s status = %s, want reserved", requestID, res.Status)
	}
}

// drain errors from ReadOnce and ClaimOnce are expected while failures are injected.
func (s streamSale) drain(t *testing.T, c *flashsale.Consumer) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.ReadOnce(ctx)
		_ = c.ClaimOnce(ctx)
		n, err := s.rdb.XLen(ctx, s.key("orders")).Result()
		if err != nil {
			t.Fatalf("xlen: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatalf("backlog for %q did not drain", s.sku)
}

func (s streamSale) rows(t *testing.T) (count, units int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(qty), 0)::bigint FROM sale_orders WHERE sku = $1`,
		s.sku).Scan(&count, &units); err != nil {
		t.Fatalf("count sale orders: %v", err)
	}
	return count, units
}

func (s streamSale) int(t *testing.T, suffix string) int64 {
	t.Helper()
	n, err := s.rdb.Get(context.Background(), s.key(suffix)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	if err != nil {
		t.Fatalf("get %s: %v", suffix, err)
	}
	return n
}

func (s streamSale) assertBalanced(t *testing.T) {
	t.Helper()
	rep, err := flashsale.Reconcile(context.Background(), s.rdb, s.pool, s.sku)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Busy || !rep.RedisBalanced() || !rep.StoresBalanced() || rep.Queued != 0 {
		t.Errorf("books do not balance: %+v", rep)
	}
}

type flakyStore struct {
	flashsale.OrderStore
	failuresLeft atomic.Int64
	err          error
}

func (s *flakyStore) InsertOrders(ctx context.Context, orders []flashsale.Order) error {
	if s.failuresLeft.Add(-1) >= 0 {
		return s.err
	}
	return s.OrderStore.InsertOrders(ctx, orders)
}

func failingStore(pool *pgxpool.Pool, failures int64, err error) *flakyStore {
	s := &flakyStore{OrderStore: flashsale.NewPGOrderStore(pool), err: err}
	s.failuresLeft.Store(failures)
	return s
}

var (
	errPermanent = &pgconn.PgError{Code: "23514", Message: "injected check violation"}
	errTransient = errors.New("injected: connection reset by peer")
)

// TestStreamPersistsOrders is the end-to-end acceptance test for v3: exactly the
// winning reservations reach Postgres, once each.
func TestStreamPersistsOrders(t *testing.T) {
	const (
		stock  = 100
		buyers = 1000
	)
	s := newStreamSale(t, stock)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range buyers {
		wg.Go(func() {
			<-start
			if _, err := s.reserver.Reserve(context.Background(), flashsale.Request{
				SKU: s.sku, UserID: fmt.Sprintf("user-%d", i), RequestID: fmt.Sprintf("req-%d", i), Qty: 1,
			}); err != nil {
				t.Errorf("buyer %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()

	s.drain(t, s.consumer(flashsale.NewPGOrderStore(s.pool), time.Minute, 5))

	if count, units := s.rows(t); count != stock || units != stock {
		t.Errorf("sale_orders = %d rows / %d units, want %d / %d", count, units, stock, stock)
	}
	s.assertBalanced(t)
}

// TestStreamRecoversCrashedConsumer simulates a worker killed mid-batch, one of
// whose orders already reached Postgres before it could acknowledge.
func TestStreamRecoversCrashedConsumer(t *testing.T) {
	const stock = 10
	s := newStreamSale(t, stock)
	ctx := context.Background()

	for i := range stock {
		s.reserve(t, "req-"+strconv.Itoa(i))
	}

	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: flashsale.ConsumerGroup, Consumer: "crashed-" + s.sku,
		Streams: []string{s.key("orders"), ">"}, Count: stock,
	}).Result()
	if err != nil || len(res) != 1 || len(res[0].Messages) != stock {
		t.Fatalf("crashed consumer read = %v, %v; want %d messages", res, err, stock)
	}

	first := res[0].Messages[0]
	if err := flashsale.NewPGOrderStore(s.pool).InsertOrders(ctx, []flashsale.Order{{
		StreamID:  first.ID,
		SKU:       s.sku,
		RequestID: first.Values["request_id"].(string),
		UserID:    first.Values["user_id"].(string),
		Qty:       1,
	}}); err != nil {
		t.Fatalf("pre-insert: %v", err)
	}

	s.drain(t, s.consumer(flashsale.NewPGOrderStore(s.pool), 100*time.Millisecond, 5))

	if count, _ := s.rows(t); count != stock {
		t.Errorf("sale_orders rows = %d, want %d (no loss, no duplicate)", count, stock)
	}
	s.assertBalanced(t)
}

// TestStreamCompensatesPermanentFailure checks an order Postgres will never accept
// is given back to stock after MaxDeliveries, and can then be bought again.
func TestStreamCompensatesPermanentFailure(t *testing.T) {
	const stock = 5
	s := newStreamSale(t, stock)
	s.reserve(t, "doomed")

	c := s.consumer(failingStore(s.pool, 1<<30, errPermanent), 20*time.Millisecond, 3)
	s.drain(t, c)

	if got := c.Compensations(); got != 1 {
		t.Errorf("compensations = %d, want 1", got)
	}
	if got := s.int(t, "stock"); got != stock {
		t.Errorf("stock = %d, want %d restored", got, stock)
	}
	if got := s.int(t, "sold"); got != 0 {
		t.Errorf("sold = %d, want 0", got)
	}
	if member, _ := s.rdb.SIsMember(context.Background(), s.key("reqs"), "doomed").Result(); member {
		t.Error("request_id still marked as applied after compensation")
	}
	if n, _ := s.rdb.XLen(context.Background(), s.key("dead")).Result(); n != 1 {
		t.Errorf("dead letters = %d, want 1", n)
	}
	if count, _ := s.rows(t); count != 0 {
		t.Errorf("sale_orders rows = %d, want 0", count)
	}
	s.assertBalanced(t)

	s.reserve(t, "doomed")
}

// TestStreamRetriesTransientFailure checks an outage longer than MaxDeliveries
// delays an order but never voids it.
func TestStreamRetriesTransientFailure(t *testing.T) {
	const maxDeliveries = 2
	s := newStreamSale(t, 5)
	s.reserve(t, "patient")

	c := s.consumer(failingStore(s.pool, maxDeliveries+3, errTransient), 20*time.Millisecond, maxDeliveries)
	s.drain(t, c)

	if got := c.Compensations(); got != 0 {
		t.Errorf("compensations = %d, want 0", got)
	}
	if n, _ := s.rdb.XLen(context.Background(), s.key("dead")).Result(); n != 0 {
		t.Errorf("dead letters = %d, want 0", n)
	}
	if count, _ := s.rows(t); count != 1 {
		t.Errorf("sale_orders rows = %d, want 1", count)
	}
	s.assertBalanced(t)
}

// TestCompensationIsIdempotent checks two settlers racing on one entry restore
// stock once.
func TestCompensationIsIdempotent(t *testing.T) {
	const stock = 5
	s := newStreamSale(t, stock)
	ctx := context.Background()
	s.reserve(t, "twice")

	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: flashsale.ConsumerGroup, Consumer: "tester-" + s.sku,
		Streams: []string{s.key("orders"), ">"}, Count: 1,
	}).Result()
	if err != nil || len(res) != 1 || len(res[0].Messages) != 1 {
		t.Fatalf("read = %v, %v; want one message", res, err)
	}
	order := flashsale.Order{
		StreamID: res[0].Messages[0].ID, SKU: s.sku, RequestID: "twice", UserID: "user-twice", Qty: 1,
	}

	c := s.consumer(flashsale.NewPGOrderStore(s.pool), time.Minute, 1)
	for attempt := range 2 {
		if err := c.CompensateOrder(ctx, order, errPermanent); err != nil {
			t.Fatalf("compensate attempt %d: %v", attempt, err)
		}
	}

	if got := c.Compensations(); got != 1 {
		t.Errorf("compensations = %d, want 1", got)
	}
	if got := s.int(t, "stock"); got != stock {
		t.Errorf("stock = %d, want %d (restored exactly once)", got, stock)
	}
	if n, _ := s.rdb.XLen(ctx, s.key("dead")).Result(); n != 1 {
		t.Errorf("dead letters = %d, want 1", n)
	}
	s.assertBalanced(t)
}

// TestReconcilerFlagsLostOrder checks a missing row is reported, but only once it
// survives a second check.
func TestReconcilerFlagsLostOrder(t *testing.T) {
	s := newStreamSale(t, 5)
	ctx := context.Background()
	for i := range 3 {
		s.reserve(t, "req-"+strconv.Itoa(i))
	}
	s.drain(t, s.consumer(flashsale.NewPGOrderStore(s.pool), time.Minute, 5))

	if _, err := s.pool.Exec(ctx,
		`DELETE FROM sale_orders WHERE sku = $1 AND request_id = 'req-0'`, s.sku); err != nil {
		t.Fatalf("delete order: %v", err)
	}

	r := flashsale.NewReconciler(s.rdb, s.pool, slog.New(slog.DiscardHandler))
	if err := r.CheckSKU(ctx, s.sku); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if got := r.Mismatches(); got != 0 {
		t.Errorf("mismatches after first check = %d, want 0 (suspected only)", got)
	}
	if err := r.CheckSKU(ctx, s.sku); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if got := r.Mismatches(); got != 1 {
		t.Errorf("mismatches after second check = %d, want 1", got)
	}
}
