// Package flashsale reserves stock two ways — a Postgres conditional UPDATE
// (postgres.go) and an atomic Redis Lua script (redis.go) — behind one Reserver
// interface, so the HTTP layer and the correctness tests are written once.
package flashsale

import (
	"context"
	"errors"
	"fmt"
)

// Status values mirror the codes returned by reserve.lua; keep the two in sync.
type Status int

const (
	StatusReserved Status = 0
	// StatusDuplicate means the request_id was already applied and stock was left untouched.
	StatusDuplicate  Status = 1
	StatusSoldOut    Status = 2
	StatusUserLimit  Status = 3
	StatusUnknownSKU Status = 4
)

func (s Status) String() string {
	switch s {
	case StatusReserved:
		return "reserved"
	case StatusDuplicate:
		return "duplicate"
	case StatusSoldOut:
		return "sold_out"
	case StatusUserLimit:
		return "user_limit"
	case StatusUnknownSKU:
		return "unknown_sku"
	default:
		return fmt.Sprintf("unknown_status_%d", int(s))
	}
}

// Reserved is true for a fresh reservation and for a replay of one.
func (s Status) Reserved() bool {
	return s == StatusReserved || s == StatusDuplicate
}

// Request carries RequestID as the idempotency key: two calls sharing one
// decrement stock at most once.
type Request struct {
	SKU       string
	UserID    string
	RequestID string
	Qty       int64
}

type Result struct {
	Status    Status
	Remaining int64
}

type Stats struct {
	Remaining    int64
	Reservations int64 // distinct request_ids applied
	Buyers       int64 // distinct users holding stock
	UnitsSold    int64 // sum of reserved quantities
}

// Reserver implementations must guarantee that concurrent calls never drive
// stock below zero and never apply the same RequestID twice.
type Reserver interface {
	Name() string
	Reserve(ctx context.Context, req Request) (Result, error)
	// SeedStock (re)initialises a SKU and clears its bookkeeping.
	SeedStock(ctx context.Context, sku string, stock int64) error
	Stats(ctx context.Context, sku string) (Stats, error)
}

// ErrUnknownSKU means the SKU has never been seeded.
var ErrUnknownSKU = errors.New("flashsale: unknown sku")

// ErrBadScriptReply means reserve.lua and redis.go have drifted apart.
var ErrBadScriptReply = errors.New("flashsale: unexpected reply from reserve.lua")
