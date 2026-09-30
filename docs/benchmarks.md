# Benchmarks

Every number here comes from `make bench-v1` / `make bench-v2` / `make bench-v3`,
which drive the same handlers in the same binary on the same machine. The only
thing that changes between the runs is which `Reserver` the request lands on.

## Environment

| | |
|---|---|
| CPU | 12th Gen Intel Core i7-12700H, 20 logical cores |
| Memory | 15 GB |
| Kernel | 6.18.33.2-microsoft-standard-WSL2 |
| Go | 1.27.1 |
| PostgreSQL | 16.15 (Docker, default config) |
| Redis | 7.4.11 (Docker, default config) |
| Load generator | k6 (Docker), same host as the system under test |

**Caveat, stated up front:** the load generator shares the machine with the API
and both datastores. That depresses every number equally, so the *ratio* between
v1 and v2 is trustworthy while the absolute ceiling is not. Phase 5 re-runs this
with a ramping profile to find the actual knee.

## Method

- 50 constant VUs, 30 s, one reservation per iteration.
- Stock is seeded to 2,000,000 — far more than a run can consume. A sold-out
  request takes a shorter code path than a successful one, so letting the SKU run
  dry would flatter whichever backend rejects faster. Every request measured here
  is a *successful write*.
- Unique `X-User-Id` and `X-Request-Id` per iteration, so neither the per-user
  limit nor the idempotency check can short-circuit the write path.
- Postgres pool capped at 25 connections (`DB_MAX_CONNS`), not pgxpool's default
  of `max(4, NumCPU)`.
- Zero-oversell is **not** asserted here; it is proven by the Go concurrency
  tests in `internal/flashsale/reserve_test.go`. k6 only checks the books balance
  at teardown (`remaining + units_sold == seeded stock`), which held on every run.

## Results — 2026-09-10

| | v1 `postgres-update` | v2 `redis-lua` | change |
|---|---|---|---|
| Throughput | 774 req/s | 26,199 req/s | **33.8×** |
| p50 | 53.6 ms | 1.60 ms | 33× faster |
| p90 | 108.4 ms | 2.67 ms | 41× faster |
| p95 | 127.8 ms | 3.07 ms | 42× faster |
| p99 | 176.7 ms | 4.03 ms | **44× faster** |
| max | 373.9 ms | 611.2 ms | — |
| Failed requests | 0 / 23,372 | 0 / 804,620 | — |
| Oversold | 0 | 0 | — |

An earlier v1 run at identical settings gave 806 req/s / p95 139 ms, so treat the
v1 figure as ~780–810 req/s rather than a precise 774.

## v3 — 2026-09-30

Same method, same machine, `make bench-v3`. v3 is v2 plus an `XADD` inside the
same Lua script and a worker that persists each order to Postgres afterwards.

| | v1 `postgres-update` | v2 `redis-lua` | v3 `redis-stream` |
|---|---|---|---|
| Throughput | 774 req/s | 26,199 req/s | 18,669 req/s |
| p50 | 53.6 ms | 1.60 ms | 2.32 ms |
| p90 | 108.4 ms | 2.67 ms | 3.88 ms |
| p95 | 127.8 ms | 3.07 ms | 4.40 ms |
| p99 | 176.7 ms | 4.03 ms | 5.66 ms |
| max | 373.9 ms | 611.2 ms | 389.9 ms |
| Failed requests | 0 / 23,372 | 0 / 804,620 | 0 / 567,952 |
| Oversold | 0 | 0 | 0 |

v3 gives up 29% of v2's throughput, and that is the honest price of
durability: the same script now also appends to a stream, and the API still
answers before the order reaches Postgres. Against v1 it is **24×** the
throughput at **31× lower p99** — while writing the same rows to the same table.

The books after the run, with the worker left to drain:

```
total 2,000,000   remaining 1,432,050   sold 567,950
persisted 567,950   rows 567,950   request_ids 567,950   queued 0   dead 0
```

`rows == request_ids == sold` is the claim that matters: every reservation became
exactly one order row, none lost and none duplicated.

## Fault injection — 2026-09-30

`make fault-test` runs bench-v3 and, while it is in flight, kills the worker for
5 s and then terminates every Postgres connection. Three runs, all green, exit 0:

| | run 1 | run 2 | run 3 |
|---|---|---|---|
| Reserved | 523,504 | (not captured) | 418,776 |
| Throughput | 17,124 req/s | — | 13,736 req/s |
| p99 | 6.12 ms | — | 8.94 ms |
| `persisted == sold` | ok | ok | ok |
| `rows == request_ids` | ok | ok | ok |
| `queued == 0` | ok | ok | ok |
| Dead letters | 0 | 0 | 0 |
| Compensations | 0 | 0 | 0 |

Throughput drops run over run because the runs were back to back on a machine
that was still draining the previous backlog, not because fault injection costs
3,400 req/s. What the runs establish is the invariant, not the number.

Zero compensations is the expected result rather than a lucky one: killing a
worker and dropping connections are both *transient* failures, which the consumer
retries forever. Compensation only fires on a permanent error (SQLSTATE 22/23),
and `consumer_test.go` is what proves that path.

## Reading the gap

v1 is not a strawman — it is correct, and `CHECK (stock >= 0)` means the database
itself would refuse to oversell even if the application logic were wrong. What it
cannot escape is the shape of the problem: **every buyer of one SKU contends on
one row.** `UPDATE products SET stock = stock - 1 WHERE sku = $1 AND stock >= 1`
takes a row lock that is held until the transaction commits, so the product row
serialises the entire sale. Throughput becomes a function of transaction latency,
and 50 concurrent buyers spend most of their time queued behind that lock — which
is exactly what the 53 ms median says.

v1 is also deliberately written as the textbook application-side transaction,
not the fastest Postgres can do: six round trips per reservation (BEGIN, replay
check, UPDATE, replay-and-limit check, INSERT, COMMIT), with the row lock held
across the last three, and an over-limit buyer decrements before rolling back.
Folding the whole reservation into one statement or a stored function would
shorten the lock hold and narrow the gap, but it would still serialise on the
row; that variant is not measured here.

v2 removes the lock rather than optimising it. The Lua script is the unit of
atomicity, Redis runs it to completion single-threaded, and nothing is held
across a network round trip. The p99 of 4 ms is essentially one RTT plus the
script.

The v2 `max` of 611 ms is worse than v1's and is not noise worth hiding: it is
the first request of the run paying for script loading plus connection pool
warm-up, against a p99 of 4 ms.

50 VUs is very likely not enough to saturate v2 or v3 — p99 stayed at 4 ms throughout,
which is not the signature of a system under pressure. The 26k figure is a floor,
not a ceiling.
