package flashsale

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ConsumerGroup is the one group every worker joins, so each order is persisted by one of them.
const ConsumerGroup = "order-writers"

// skusKey lists the SKUs whose streams the workers consume. It lives outside any
// {sku} slot and is written by SeedStock rather than reserve.lua for that reason.
const skusKey = "flashsale:skus"

func totalKey(sku string) string  { return "{" + sku + "}:total" }
func queuedKey(sku string) string { return "{" + sku + "}:queued" }
func ordersKey(sku string) string { return "{" + sku + "}:orders" }
func deadKey(sku string) string   { return "{" + sku + "}:dead" }

func streamKeysFor(sku string) []string {
	return append(keysFor(sku), queuedKey(sku), ordersKey(sku))
}

// Order is one reservation as carried on {sku}:orders and stored in sale_orders.
type Order struct {
	StreamID  string
	SKU       string
	RequestID string
	UserID    string
	Qty       int64
}

// errPoisonMessage marks a stream entry that cannot be decoded into an Order.
var errPoisonMessage = errors.New("flashsale: malformed order message")

func decodeOrder(sku string, msg redis.XMessage) (Order, error) {
	requestID, ok1 := msg.Values["request_id"].(string)
	userID, ok2 := msg.Values["user_id"].(string)
	rawQty, ok3 := msg.Values["qty"].(string)
	if !ok1 || !ok2 || !ok3 || requestID == "" || userID == "" {
		return Order{}, fmt.Errorf("%w: %s", errPoisonMessage, msg.ID)
	}
	qty, err := strconv.ParseInt(rawQty, 10, 64)
	if err != nil || qty < 1 {
		return Order{}, fmt.Errorf("%w: %s has qty %q", errPoisonMessage, msg.ID, rawQty)
	}
	return Order{StreamID: msg.ID, SKU: sku, RequestID: requestID, UserID: userID, Qty: qty}, nil
}

// StreamReserver is the v3 path: the same atomic reservation as RedisReserver,
// plus an order message published in the same script for a worker to persist.
type StreamReserver struct {
	*RedisReserver
	pool *pgxpool.Pool
}

var _ Reserver = (*StreamReserver)(nil)

func NewStreamReserver(rdb redis.Cmdable, pool *pgxpool.Pool, perUserLimit int64, keyTTL time.Duration) *StreamReserver {
	return &StreamReserver{RedisReserver: NewRedisReserver(rdb, perUserLimit, keyTTL), pool: pool}
}

func (s *StreamReserver) Name() string { return "redis-stream" }

func (s *StreamReserver) Reserve(ctx context.Context, req Request) (Result, error) {
	return s.reserve(ctx, streamKeysFor(req.SKU), req)
}

// SeedStock is destructive by design: it drops the backlog, the dead letters and
// every persisted order for the SKU. Reseeding while a worker holds a batch can
// still let that batch land afterwards, so seed a SKU only while it is idle.
func (s *StreamReserver) SeedStock(ctx context.Context, sku string, stock int64) error {
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, stockKey(sku), stock, s.keyTTL)
	pipe.Set(ctx, totalKey(sku), stock, s.keyTTL)
	pipe.Del(ctx, usersKey(sku), reqsKey(sku), soldKey(sku), queuedKey(sku), ordersKey(sku), deadKey(sku))
	pipe.XGroupCreateMkStream(ctx, ordersKey(sku), ConsumerGroup, "0")
	if s.keyTTL > 0 {
		pipe.PExpire(ctx, ordersKey(sku), s.keyTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("seed stock for %q: %w", sku, err)
	}

	if _, err := s.pool.Exec(ctx, `DELETE FROM sale_orders WHERE sku = $1`, sku); err != nil {
		return fmt.Errorf("clear sale orders for %q: %w", sku, err)
	}

	// Registered last so a worker never polls a stream whose group is not there yet.
	if err := s.rdb.SAdd(ctx, skusKey, sku).Err(); err != nil {
		return fmt.Errorf("register %q for workers: %w", sku, err)
	}
	return nil
}
