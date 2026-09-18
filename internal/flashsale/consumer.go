package flashsale

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

//go:embed ack.lua
var ackSrc string

//go:embed compensate.lua
var compensateSrc string

// OrderStore implementations must treat an order already stored under its
// (sku, request_id) as success, because redelivery is part of normal operation.
type OrderStore interface {
	InsertOrders(ctx context.Context, orders []Order) error
}

type PGOrderStore struct {
	pool *pgxpool.Pool
}

var _ OrderStore = (*PGOrderStore)(nil)

func NewPGOrderStore(pool *pgxpool.Pool) *PGOrderStore { return &PGOrderStore{pool: pool} }

// InsertOrders writes the whole batch in one statement.
func (s *PGOrderStore) InsertOrders(ctx context.Context, orders []Order) error {
	n := len(orders)
	requestIDs, skus, userIDs, streamIDs := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	qtys := make([]int64, n)
	for i, o := range orders {
		requestIDs[i], skus[i], userIDs[i], qtys[i], streamIDs[i] = o.RequestID, o.SKU, o.UserID, o.Qty, o.StreamID
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sale_orders (request_id, sku, user_id, qty, stream_id)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::text[])
		ON CONFLICT (sku, request_id) DO NOTHING`,
		requestIDs, skus, userIDs, qtys, streamIDs); err != nil {
		return fmt.Errorf("insert %d sale orders: %w", n, err)
	}
	return nil
}

// isPermanent reports whether retrying the same order can never succeed. Anything
// unrecognised counts as transient, so an unknown failure delays an order rather
// than voiding a purchase.
func isPermanent(err error) bool {
	if errors.Is(err, errPoisonMessage) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	// Class 22 is invalid data, class 23 an integrity constraint violation.
	return strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")
}

type ConsumerConfig struct {
	// Name identifies this consumer inside the group and must be unique per process.
	Name      string
	BatchSize int64
	// ClaimIdle is how long an entry may stay unacknowledged before it is taken over.
	ClaimIdle time.Duration
	// MaxDeliveries is how many times a permanently failing order is tried before compensation.
	MaxDeliveries int64
	// Block is how long one XREADGROUP waits for new entries.
	Block time.Duration
	// Refresh is how often the SKU registry is re-read.
	Refresh time.Duration
	// SKUs pins the consumer to a fixed set of SKUs instead of following the registry.
	SKUs []string
}

const (
	// settleTimeout bounds how long a batch already read may outlive shutdown.
	settleTimeout = 5 * time.Second
	maxBackoff    = 5 * time.Second
)

// Consumer persists the orders StreamReserver publishes. It is safe to run many
// consumers in one group: every settle path is guarded by XACK returning 1.
type Consumer struct {
	rdb        redis.Cmdable
	store      OrderStore
	cfg        ConsumerConfig
	logger     *slog.Logger
	ack        *redis.Script
	compensate *redis.Script

	compensations atomic.Int64

	mu          sync.Mutex
	skus        []string
	refreshedAt time.Time
}

func NewConsumer(rdb redis.Cmdable, store OrderStore, cfg ConsumerConfig, logger *slog.Logger) *Consumer {
	if cfg.Name == "" {
		cfg.Name = "worker"
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 100
	}
	if cfg.ClaimIdle <= 0 {
		cfg.ClaimIdle = 30 * time.Second
	}
	if cfg.MaxDeliveries < 1 {
		cfg.MaxDeliveries = 1
	}
	if cfg.Block <= 0 {
		cfg.Block = 2 * time.Second
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 2 * time.Second
	}
	return &Consumer{
		rdb:        rdb,
		store:      store,
		cfg:        cfg,
		logger:     logger,
		ack:        redis.NewScript(ackSrc),
		compensate: redis.NewScript(compensateSrc),
	}
}

// Compensations counts orders this consumer gave back to stock.
func (c *Consumer) Compensations() int64 { return c.compensations.Load() }

// Run blocks until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { c.readLoop(ctx) })
	wg.Go(func() { c.claimLoop(ctx) })
	wg.Wait()
}

func (c *Consumer) readLoop(ctx context.Context) {
	var bo backoff
	for ctx.Err() == nil {
		if err := c.ReadOnce(ctx); err != nil {
			c.logger.Warn("persist new orders failed", "consumer", c.cfg.Name, "err", err)
			bo.wait(ctx)
			continue
		}
		bo.reset()
	}
}

func (c *Consumer) claimLoop(ctx context.Context) {
	ticker := time.NewTicker(max(c.cfg.ClaimIdle/2, 10*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := c.ClaimOnce(ctx); err != nil {
			c.logger.Warn("persist reclaimed orders failed", "consumer", c.cfg.Name, "err", err)
		}
	}
}

// ReadOnce reads at most one batch of new entries per stream and settles them.
func (c *Consumer) ReadOnce(ctx context.Context) error {
	skus, err := c.registered(ctx)
	if err != nil {
		return err
	}
	if len(skus) == 0 {
		sleepCtx(ctx, c.cfg.Block)
		return nil
	}

	skuOf := make(map[string]string, len(skus))
	streams := make([]string, 0, 2*len(skus))
	for _, sku := range skus {
		skuOf[ordersKey(sku)] = sku
		streams = append(streams, ordersKey(sku))
	}
	for range skus {
		streams = append(streams, ">")
	}

	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: c.cfg.Name,
		Streams:  streams,
		Count:    c.cfg.BatchSize,
		Block:    c.cfg.Block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		// One expired stream fails the whole multi-stream read until the registry drops it.
		if strings.HasPrefix(err.Error(), "NOGROUP") {
			c.invalidate()
		}
		return fmt.Errorf("read order streams: %w", err)
	}

	work, cancel := settleContext(ctx)
	defer cancel()
	var errs []error
	for _, stream := range res {
		if err := c.settle(work, skuOf[stream.Stream], stream.Messages, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ClaimOnce takes over every entry idle for longer than ClaimIdle and settles it.
func (c *Consumer) ClaimOnce(ctx context.Context) error {
	skus, err := c.registered(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, sku := range skus {
		if err := c.claimStream(ctx, sku); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Consumer) claimStream(ctx context.Context, sku string) error {
	start := "0-0"
	for ctx.Err() == nil {
		msgs, next, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   ordersKey(sku),
			Group:    ConsumerGroup,
			MinIdle:  c.cfg.ClaimIdle,
			Start:    start,
			Count:    c.cfg.BatchSize,
			Consumer: c.cfg.Name,
		}).Result()
		if err != nil {
			return fmt.Errorf("claim idle orders for %q: %w", sku, err)
		}

		if len(msgs) > 0 {
			deliveries, err := c.deliveries(ctx, sku, msgs)
			if err != nil {
				return err
			}
			work, cancel := settleContext(ctx)
			err = c.settle(work, sku, msgs, deliveries)
			cancel()
			if err != nil {
				return err
			}
		}

		if next == "0-0" {
			return nil
		}
		start = next
	}
	return nil
}

// deliveries reads delivery counts, which XAUTOCLAIM increments but does not return.
func (c *Consumer) deliveries(ctx context.Context, sku string, msgs []redis.XMessage) (map[string]int64, error) {
	pending, err := c.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: ordersKey(sku),
		Group:  ConsumerGroup,
		Start:  msgs[0].ID,
		End:    msgs[len(msgs)-1].ID,
		// Entries this consumer is still settling from ReadOnce can share the range.
		Count:    int64(len(msgs)) + c.cfg.BatchSize,
		Consumer: c.cfg.Name,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("read delivery counts for %q: %w", sku, err)
	}
	counts := make(map[string]int64, len(pending))
	for _, p := range pending {
		counts[p.ID] = p.RetryCount
	}
	return counts, nil
}

// settle persists msgs and acknowledges what landed. Entries missing from
// deliveries count as first deliveries.
func (c *Consumer) settle(ctx context.Context, sku string, msgs []redis.XMessage, deliveries map[string]int64) error {
	orders := make([]Order, 0, len(msgs))
	for _, msg := range msgs {
		order, err := decodeOrder(sku, msg)
		if err != nil {
			if err := c.deadLetter(ctx, sku, msg, err); err != nil {
				return err
			}
			continue
		}
		orders = append(orders, order)
	}
	if len(orders) == 0 {
		return nil
	}

	err := c.store.InsertOrders(ctx, orders)
	if err == nil {
		c.logPersisted(ctx, sku, orders)
		return c.acknowledge(ctx, sku, orders)
	}
	if !isPermanent(err) {
		return fmt.Errorf("persist %d orders for %q: %w", len(orders), sku, err)
	}

	// One bad row fails the whole statement, so go row by row to let the rest through.
	var persisted []Order
	var errs []error
	for _, order := range orders {
		err := c.store.InsertOrders(ctx, []Order{order})
		switch {
		case err == nil:
			persisted = append(persisted, order)
		case !isPermanent(err):
			errs = append(errs, fmt.Errorf("persist order %s for %q: %w", order.StreamID, sku, err))
		case deliveryCount(deliveries, order.StreamID) >= c.cfg.MaxDeliveries:
			if err := c.compensateOrder(ctx, order, err); err != nil {
				errs = append(errs, err)
			}
		default:
			// Left pending on purpose: ClaimOnce redelivers it after ClaimIdle.
			c.logger.Warn("order rejected, will retry", "consumer", c.cfg.Name, "sku", sku,
				"request_id", order.RequestID, "stream_id", order.StreamID,
				"deliveries", deliveryCount(deliveries, order.StreamID), "err", err)
		}
	}
	if err := c.acknowledge(ctx, sku, persisted); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func deliveryCount(deliveries map[string]int64, id string) int64 {
	if n, ok := deliveries[id]; ok {
		return n
	}
	return 1
}

// logPersisted is what lets one request_id be followed from the api to the row it
// became. It is guarded because a batch holds up to BatchSize orders.
func (c *Consumer) logPersisted(ctx context.Context, sku string, orders []Order) {
	if !c.logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	for _, order := range orders {
		c.logger.Debug("order persisted", "consumer", c.cfg.Name, "sku", sku,
			"request_id", order.RequestID, "user_id", order.UserID, "qty", order.Qty,
			"stream_id", order.StreamID)
	}
}

func (c *Consumer) acknowledge(ctx context.Context, sku string, orders []Order) error {
	if len(orders) == 0 {
		return nil
	}
	args := make([]any, 0, 1+2*len(orders))
	args = append(args, ConsumerGroup)
	for _, o := range orders {
		args = append(args, o.StreamID, o.Qty)
	}
	if err := c.ack.Run(ctx, c.rdb, []string{ordersKey(sku), queuedKey(sku)}, args...).Err(); err != nil {
		return fmt.Errorf("acknowledge %d orders for %q: %w", len(orders), sku, err)
	}
	return nil
}

func (c *Consumer) compensateOrder(ctx context.Context, order Order, cause error) error {
	sku := order.SKU
	keys := []string{
		stockKey(sku), usersKey(sku), reqsKey(sku), soldKey(sku),
		queuedKey(sku), ordersKey(sku), deadKey(sku),
	}
	settled, err := c.compensate.Run(ctx, c.rdb, keys, ConsumerGroup,
		order.StreamID, order.RequestID, order.UserID, order.Qty, cause.Error()).Int64()
	if err != nil {
		return fmt.Errorf("compensate order %s for %q: %w", order.StreamID, sku, err)
	}
	if settled == 1 {
		c.compensations.Add(1)
		c.logger.Error("order compensated and dead-lettered", "consumer", c.cfg.Name, "sku", sku,
			"request_id", order.RequestID, "user_id", order.UserID, "qty", order.Qty,
			"stream_id", order.StreamID, "err", cause)
	}
	return nil
}

// deadLetter parks an entry that cannot be decoded. Its qty is unknown, so stock
// and queued are left alone and the reconciler will report the SKU.
func (c *Consumer) deadLetter(ctx context.Context, sku string, msg redis.XMessage, cause error) error {
	pipe := c.rdb.TxPipeline()
	pipe.XAck(ctx, ordersKey(sku), ConsumerGroup, msg.ID)
	pipe.XDel(ctx, ordersKey(sku), msg.ID)
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: deadKey(sku),
		Values: map[string]any{"stream_id": msg.ID, "reason": cause.Error(), "restored": 0},
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("dead-letter %s for %q: %w", msg.ID, sku, err)
	}
	c.logger.Error("malformed order dead-lettered", "consumer", c.cfg.Name, "sku", sku,
		"stream_id", msg.ID, "err", cause)
	return nil
}

func (c *Consumer) registered(ctx context.Context) ([]string, error) {
	if len(c.cfg.SKUs) > 0 {
		return c.cfg.SKUs, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.refreshedAt.IsZero() && time.Since(c.refreshedAt) < c.cfg.Refresh {
		return c.skus, nil
	}
	skus, err := RegisteredSKUs(ctx, c.rdb)
	if err != nil {
		return nil, err
	}
	c.skus, c.refreshedAt = skus, time.Now()
	return skus, nil
}

func (c *Consumer) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshedAt = time.Time{}
}

// RegisteredSKUs prunes SKUs whose stream has expired, which is how the registry
// forgets finished sales.
func RegisteredSKUs(ctx context.Context, rdb redis.Cmdable) ([]string, error) {
	skus, err := rdb.SMembers(ctx, skusKey).Result()
	if err != nil {
		return nil, fmt.Errorf("read sku registry: %w", err)
	}
	if len(skus) == 0 {
		return nil, nil
	}

	pipe := rdb.Pipeline()
	exists := make([]*redis.IntCmd, len(skus))
	for i, sku := range skus {
		exists[i] = pipe.Exists(ctx, ordersKey(sku))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("check registered streams: %w", err)
	}

	live := make([]string, 0, len(skus))
	var gone []any
	for i, sku := range skus {
		if exists[i].Val() == 1 {
			live = append(live, sku)
		} else {
			gone = append(gone, sku)
		}
	}
	if len(gone) > 0 {
		if err := rdb.SRem(ctx, skusKey, gone...).Err(); err != nil {
			return nil, fmt.Errorf("prune sku registry: %w", err)
		}
	}
	slices.Sort(live)
	return live, nil
}

// settleContext lets a batch already read finish after shutdown instead of
// leaving it for another worker to reclaim ClaimIdle later.
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}

// backoff keeps a worker from spinning against a database that is down.
type backoff struct {
	delay time.Duration
}

func (b *backoff) reset() { b.delay = 0 }

func (b *backoff) wait(ctx context.Context) {
	b.delay = min(max(2*b.delay, 100*time.Millisecond), maxBackoff)
	sleepCtx(ctx, b.delay)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
