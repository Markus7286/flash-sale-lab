package flashsale

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed reserve.lua
var reserveSrc string

// RedisReserver runs the whole reservation inside one Lua script, so every check
// and the decrement commit together without a lock held across a round trip.
type RedisReserver struct {
	rdb          redis.Cmdable
	script       *redis.Script
	perUserLimit int64
	keyTTL       time.Duration
}

var _ Reserver = (*RedisReserver)(nil)

// NewRedisReserver caps each user at perUserLimit units of one SKU. A positive
// keyTTL expires a seeded SKU together with its bookkeeping; zero keeps it forever.
func NewRedisReserver(rdb redis.Cmdable, perUserLimit int64, keyTTL time.Duration) *RedisReserver {
	if perUserLimit < 1 {
		perUserLimit = 1
	}
	// go-redis reads a negative expiration as KEEPTTL, which is not "no TTL".
	if keyTTL < 0 {
		keyTTL = 0
	}
	return &RedisReserver{
		rdb:          rdb,
		script:       redis.NewScript(reserveSrc),
		perUserLimit: perUserLimit,
		keyTTL:       keyTTL,
	}
}

func (r *RedisReserver) Name() string { return "redis-lua" }

// The {sku} hash tag keeps every key in one slot, without which the script and
// the MULTI in Stats would be illegal on a Redis Cluster.
func stockKey(sku string) string { return "{" + sku + "}:stock" }
func usersKey(sku string) string { return "{" + sku + "}:users" }
func reqsKey(sku string) string  { return "{" + sku + "}:reqs" }
func soldKey(sku string) string  { return "{" + sku + "}:sold" }

func keysFor(sku string) []string {
	return []string{stockKey(sku), usersKey(sku), reqsKey(sku), soldKey(sku)}
}

// Reserve sends EVALSHA and falls back to EVAL only on NOSCRIPT, so the hot path
// carries a 40-byte digest rather than the whole script.
func (r *RedisReserver) Reserve(ctx context.Context, req Request) (Result, error) {
	return r.reserve(ctx, keysFor(req.SKU), req)
}

func (r *RedisReserver) reserve(ctx context.Context, keys []string, req Request) (Result, error) {
	if req.Qty < 1 {
		req.Qty = 1
	}

	raw, err := r.script.Run(ctx, r.rdb, keys,
		req.UserID, req.RequestID, req.Qty, r.perUserLimit).Result()
	if err != nil {
		return Result{}, fmt.Errorf("run reserve.lua: %w", err)
	}

	reply, ok := raw.([]any)
	if !ok || len(reply) != 2 {
		return Result{}, ErrBadScriptReply
	}
	status, ok1 := reply[0].(int64)
	remaining, ok2 := reply[1].(int64)
	if !ok1 || !ok2 {
		return Result{}, ErrBadScriptReply
	}

	return Result{Status: Status(status), Remaining: remaining}, nil
}

// SeedStock is destructive by design: it drops the per-user and idempotency keys.
func (r *RedisReserver) SeedStock(ctx context.Context, sku string, stock int64) error {
	pipe := r.rdb.TxPipeline()
	pipe.Set(ctx, stockKey(sku), stock, r.keyTTL)
	pipe.Del(ctx, usersKey(sku), reqsKey(sku), soldKey(sku))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("seed stock for %q: %w", sku, err)
	}
	return nil
}

// Stats reads every key inside one MULTI, so a reservation cannot land between
// the reads and leave remaining and units sold out of step.
func (r *RedisReserver) Stats(ctx context.Context, sku string) (Stats, error) {
	pipe := r.rdb.TxPipeline()
	stockCmd := pipe.Get(ctx, stockKey(sku))
	soldCmd := pipe.Get(ctx, soldKey(sku))
	buyersCmd := pipe.HLen(ctx, usersKey(sku))
	reqsCmd := pipe.SCard(ctx, reqsKey(sku))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return Stats{}, fmt.Errorf("read stats for %q: %w", sku, err)
	}

	remaining, err := stockCmd.Int64()
	if errors.Is(err, redis.Nil) {
		return Stats{}, ErrUnknownSKU
	}
	if err != nil {
		return Stats{}, fmt.Errorf("read stock for %q: %w", sku, err)
	}

	sold, err := soldCmd.Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Stats{}, fmt.Errorf("read units sold for %q: %w", sku, err)
	}

	return Stats{
		Remaining:    remaining,
		Reservations: reqsCmd.Val(),
		Buyers:       buyersCmd.Val(),
		UnitsSold:    sold,
	}, nil
}
