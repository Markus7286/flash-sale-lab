# Benchmarks

Every number here comes from a `make` target driving the same handlers in the
same binary on the same machine. The only thing that changes between runs is
which `Reserver` the request lands on.

| | profile | what it answers |
|---|---|---|
| `make bench-v1/v2/v3` | 50 VUs, 30 s | throughput at fixed concurrency |
| `make ramp` | 10 steps of 2,000/s | where is the knee? |
| `make spike` | 500/s → 15,000/s → 500/s | cost of a surge, and recovery |
| `make soak` | 8,000/s for 30 min | does anything drift? |
| `make fault-test` | bench-v3 + injected failures | are the books still right? |

All four load profiles share one request definition (`k6/lib/sale.js`), so a ramp
is comparable to a baseline.

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

Common to every profile:

- One reservation per iteration. The baseline runs 50 constant VUs for 30 s; the
  ramp, spike and soak drive an **arrival rate** instead, because a constant-VU
  test cannot overload a service — slower responses simply mean fewer requests.
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

## All three backends — 2026-09-30

The 2026-09-10 table above compares v1 and v2 but predates v3 and the
observability stack. This section re-runs all three on one afternoon, back to
back, alternating versions so machine load lands on every version equally.
Three runs each; the spread is the full range observed.

| | v1 `postgres-update` | v2 `redis-lua` | v3 `redis-stream` |
|---|---|---|---|
| Throughput | 434–491 req/s | 15,122–15,554 req/s | 10,972–11,308 req/s |
| p50 | 102.6 ms | 2.73 ms | 3.86 ms |
| p90 | 181.3 ms | 5.03 ms | 6.59 ms |
| p95 | 216.6 ms | 5.86 ms | 7.44 ms |
| p99 | 272.8–302.9 ms | 7.61–7.64 ms | 9.24–9.60 ms |
| max | 668.1 ms | 159.2 ms | 329.2 ms |
| Failed requests | 0 | 0 | 0 |
| Oversold | 0 | 0 | 0 |

Quantiles are from the third run of each; throughput and p99 give the range
across all three. Run-to-run spread is under 3% for v2 and v3.

**Every absolute number here is lower than 2026-09-10** — v1 fell from 774 to
~445 req/s, v2 from 26,199 to ~15,300. Nothing regressed in the code. The
machine now also runs Prometheus and Grafana, and Prometheus scrapes both
binaries every 5 s, which is the price of Phase 4 being switched on. The
v2-over-v1 ratio is **34.9×** today against 33.8× on 2026-09-10, which is the
caveat at the top of this file being borne out: the ratio travels, the ceiling
does not.

### What v3 costs

v3 does everything v2 does, plus an `XADD` in the same Lua script, plus a worker
persisting each order to Postgres. It runs at **73% of v2's throughput** and
about 1.2 ms behind it at p99. Against v1 it is still **26×** the throughput at
**33× lower p99** — while writing the same rows to the same table v1 writes
synchronously.

An early isolated v3 run measured 18,669 req/s, which is higher than any v2 run
here. That number is not in the table and should not be quoted: it was the first
run after `make up`, against an empty Redis with no prior backlog draining, and
nothing else had run on the box. Paired alternating runs are the only comparison
this file trusts, and they put v3 consistently below v2.

### The books

From the 18,669 req/s run, after the worker was left to drain:

```
total 2,000,000   remaining 1,432,050   sold 567,950
persisted 567,950   rows 567,950   request_ids 567,950   queued 0   dead 0
```

`rows == request_ids == sold` is the claim that matters: every reservation became
exactly one order row, none lost and none duplicated. It held on every v3 run.

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

These runs are not comparable to the table above either — they were consecutive,
each starting while the previous backlog was still draining. What they establish
is the invariant, not the number.

Zero compensations is the expected result rather than a lucky one: killing a
worker and dropping connections are both *transient* failures, which the consumer
retries forever. Compensation only fires on a permanent error (SQLSTATE 22/23),
and `consumer_test.go` is what proves that path.

## Ramp — 2026-09-30

`make ramp`. Ten steps of 2,000/s, 20 s each, `MAX_VUS=4000`. Arrival rate rather
than VU count: a constant-VU test cannot overload a service, because slower
responses just mean fewer requests. Every column below is that step's own
sub-metric, not a whole-run aggregate.

### v1 `postgres-update`

Steps of 100/s rather than 2,000/s, `MAX_VUS=2000`.

| arrival | achieved | p50 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|
| 100/s | 100/s | 1.71 ms | 3.55 ms | 4.44 ms | 7.86 ms | 0% |
| 200/s | 200/s | 1.52 ms | 2.39 ms | 3.38 ms | 73.78 ms | 0% |
| 300/s | 300/s | 1.48 ms | 2.06 ms | 3.03 ms | 75.59 ms | 0% |
| 400/s | 400/s | 1.44 ms | 2.03 ms | 3.24 ms | 9.03 ms | 0% |
| 500/s | 500/s | 1.43 ms | 1.88 ms | 2.80 ms | 73.15 ms | 0% |
| 600/s | 600/s | 1.45 ms | 1.98 ms | 3.15 ms | 67.24 ms | 0% |
| 700/s | 700/s | 1.39 ms | 2.01 ms | 4.26 ms | 30.35 ms | 0% |
| 800/s | **769/s** | 358 ms | 863 ms | 944 ms | 1,273 ms | 0% |
| 900/s | 749/s | 1,955 ms | 2,909 ms | 3,015 ms | 3,308 ms | 0% |
| 1,000/s | 661/s | 2,782 ms | 3,251 ms | 3,344 ms | 3,737 ms | 0% |

**This changes the story the 2026-09-10 table tells.** v1 serves 700 req/s at a
p99 of 4.26 ms — it is not a slow implementation, it is a narrow one. Seven steps
pass with latency flat or falling, then one step past the limit the median is
358 ms and two steps past it two seconds. The row lock either absorbs the offered
concurrency or it does not.

The 50-VU baseline reports v1 at ~450 req/s and p99 290 ms because 50 concurrent
buyers offer roughly 20× what the lock can absorb: that run measures v1 in
collapse, not v1 working. Both readings are real; they answer different questions.

`Insufficient VUs` appeared at steps 9 and 10, so those two arrival figures are
nominal. Step 8 — the first failing step, and the one the knee rests on — is clean.

### v2 `redis-lua`

| arrival | achieved | p50 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|
| 2,000/s | 2,000/s | 0.63 ms | 1.22 ms | 2.02 ms | 48.85 ms | 0% |
| 4,000/s | 3,991/s | 0.73 ms | 1.59 ms | 5.95 ms | 24.90 ms | 0% |
| 6,000/s | 5,976/s | 0.82 ms | 2.62 ms | 8.91 ms | 49.53 ms | 0% |
| 8,000/s | 7,954/s | 1.05 ms | 6.15 ms | 14.31 ms | 42.45 ms | 0% |
| 10,000/s | 9,839/s | 2.01 ms | 14.43 ms | 25.25 ms | 52.20 ms | 0% |
| 12,000/s | 11,694/s | 2.63 ms | 18.37 ms | 32.24 ms | 68.03 ms | 0% |
| 14,000/s | 13,295/s | 4.89 ms | 36.04 ms | 51.76 ms | 104 ms | 0% |
| 16,000/s | **14,269/s** | 41.59 ms | 76.87 ms | 99.67 ms | 193 ms | 0% |
| 18,000/s | 13,740/s | 62.13 ms | 161 ms | 198 ms | 463 ms | 0% |
| 20,000/s | 12,498/s | 101 ms | 266 ms | 380 ms | 791 ms | 0% |

### v3 `redis-stream`

| arrival | achieved | p50 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|
| 2,000/s | 2,000/s | 0.74 ms | 1.31 ms | 2.10 ms | 28.85 ms | 0% |
| 4,000/s | 3,995/s | 0.99 ms | 2.46 ms | 7.29 ms | 25.02 ms | 0% |
| 6,000/s | 5,971/s | 1.30 ms | 5.78 ms | 13.44 ms | 51.86 ms | 0% |
| 8,000/s | 7,925/s | 2.70 ms | 12.90 ms | 21.81 ms | 45.35 ms | 0% |
| 10,000/s | 9,720/s | 6.47 ms | 30.90 ms | 42.03 ms | 81.66 ms | 0% |
| 12,000/s | **11,275/s** | 20.08 ms | 55.90 ms | 70.89 ms | 143 ms | 0% |
| 14,000/s | 10,399/s | 135 ms | 295 ms | 377 ms | 576 ms | 0% |
| 16,000/s | 9,496/s | 285 ms | 450 ms | 532 ms | 607 ms | 0% |
| 18,000/s | 9,068/s | 291 ms | 472 ms | 558 ms | 962 ms | 0% |
| 20,000/s | 10,502/s | 324 ms | 499 ms | 622 ms | 757 ms | 0% |

### What the ramp says

Neither version plateaus — both **collapse**. Past the peak, offering more load
returns less throughput and much worse latency: v2 peaks at 14,269/s and is down
to 12,498/s when offered 20,000/s, with p99 nearly 4× worse. A fixed-VU benchmark
cannot show this, because it cannot offer more load than the service accepts.

Sustainable rate — achieved still within 3% of arrival, p99 still double-digit —
is about **700/s for v1, 12,000/s for v2 and 10,000/s for v3**. Compared that way
v2 is **17× v1**, not the 34.9× the fixed-concurrency runs report; the larger
figure credits v2 for v1 collapsing rather than for v2 scaling.

Overload behaviour is the other half of the comparison, and it separates them
further than throughput does. Pushed past its knee, v1's p99 reaches 3,344 ms —
roughly 780× its healthy p99. v2 reaches 380 ms, about 12× its own. Both collapse;
one collapses gently.

The v3 collapse is sharper and earlier. Its extra work per request is an `XADD`
in the same script, so Redis does strictly more per reservation on the one core
that owns the SKU's hash slot.

**Two caveats.** From step 8 (v3) and step 9 (v2) onward k6 reported
`Insufficient VUs`, so the *arrival* column is nominal beyond those rows — the
generator could not offer the full rate. The *achieved* column is measured
either way, and achieved throughput falling while VU count climbs is the service
saturating rather than the generator. Second, k6 runs on the same machine, so
above ~14,000/s it is competing with the API for CPU.

### Writer throughput

The v3 ramp put 1,607,025 reservations through the stream. When it ended the
backlog stood at 719,175 orders and drained to zero in 53 s, so the worker
sustains roughly **13,500 inserts/s** — above v3's own request ceiling, which is
why a backlog always drains rather than growing without bound. All 1,607,025
orders persisted, `sold == persisted`, `queued == 0`.

## Spike — 2026-09-30

`make spike`. 500/s for 20 s, an instant step to 15,000/s for 30 s, then 500/s
again. Three back-to-back `constant-arrival-rate` scenarios rather than a ramp,
so nothing gets a warm-up: not the connection pool, not the Lua script cache.

| | arrival | achieved | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| **v2** calm | 500/s | 500/s | 0.64 ms | 1.21 ms | 1.54 ms | 6.18 ms |
| **v2** spike | 15,000/s | 14,311/s | 17.68 ms | 43.22 ms | 63.34 ms | 133 ms |
| **v2** recover | 500/s | 500/s | 0.61 ms | 1.15 ms | **1.53 ms** | 28.17 ms |
| **v3** calm | 500/s | 500/s | 0.85 ms | 1.39 ms | 1.70 ms | 4.87 ms |
| **v3** spike | 15,000/s | 11,201/s | 235 ms | 408 ms | 480 ms | 632 ms |
| **v3** recover | 500/s | 498/s | 0.85 ms | 1.98 ms | **9.46 ms** | 240 ms |

Zero failed requests in both, at 30× the calm rate.

v2 returns to its calm p99 to the hundredth of a millisecond. v3 comes back to
9.46 ms rather than 1.70 ms, and the reason is visible on the dashboard: the
worker is still draining the spike's backlog while the recovery phase runs, so
Redis is still serving `XREADGROUP` and `ack.lua` alongside the new traffic. The
reservation path recovered immediately; the write path had not finished catching
up. p50 is already back to 0.85 ms, so this is a tail effect, not a slowdown.

The 15,000/s target was chosen before the ramp existed and is above both
versions' ceilings, which makes this an overload test as well as a spike test.
That is the more useful question anyway: a sale opening does not ask permission.

## Soak — 2026-09-30

`make soak`, v3 at 8,000/s. 10 minutes; the first ~90 s is the executor ramping
VUs, so the measurement window is the 9 minutes of flat 7,989 req/s from 15:52:00
to 16:01:00 UTC, 4,308,338 requests. Values below are from Prometheus at 30 s
resolution, comparing the mean of the first three samples against the last three.

| | min | max | first 90 s | last 90 s | drift |
|---|---|---|---|---|---|
| p50 | 0.32 ms | 0.42 ms | 0.37 ms | 0.37 ms | −0.01 ms |
| p95 | 0.97 ms | 2.96 ms | 1.70 ms | 1.50 ms | −0.20 ms |
| p99 | 2.35 ms | 8.29 ms | 4.33 ms | 3.49 ms | −0.84 ms |
| Throughput | 7,953/s | 7,998/s | 7,987/s | 7,993/s | +6/s |
| Write backlog | 25 | 77 | 36 | 39 | +3 |
| API RSS | 100.2 MiB | 109.6 MiB | 101.4 MiB | 104.8 MiB | +3.4 MiB |
| Worker RSS | 22.5 MiB | 23.6 MiB | 22.9 MiB | 23.0 MiB | +0.1 MiB |
| Failed requests | 0 | 0 | 0 | 0 | — |

Nothing drifts. p99 *improves* slightly, which is a warm connection pool and a
loaded script cache rather than a mystery. The backlog oscillates between 25 and
77 orders across 4.3M insertions and never trends upward: the writer stays ahead
of the producer for the whole run.

Books at the end, after the last batch drained:

```
total 30,000,000   remaining 25,123,676   sold 4,876,324
persisted 4,876,324   rows 4,876,324   duplicate request_ids 0
queued 0   stream 0   dead 0   compensations 0   reconcile mismatches 0
```

### The one thing it surfaced

API goroutines step from 171 to 491 about two minutes in and hold there for the
rest of the run, which looks like a leak until you watch what happens at the end:
they return to 10. The step is k6's arrival-rate executor allocating VUs to hold
8,000/s, and `net/http` running a goroutine per connection. The spike test's peak
of 6,781 goroutines collapsed to 10 the same way. Worth recording precisely
because the shape is the same as a leak and only the recovery distinguishes them.

### Caveat on the duration

This is **10 minutes, not 30**. The profile default was cut to 10 so that the
target actually gets run; `make soak DURATION=30m` still exists and no 30-minute
run is recorded in this file. A 10-minute window at 8,000/s is 4.3M requests and
is enough to rule out per-request leaks and backlog growth. It is not enough to
rule out something with an hourly period.

## Reading the gap

v1 is not a strawman — it is correct, and `CHECK (stock >= 0)` means the database
itself would refuse to oversell even if the application logic were wrong. What it
cannot escape is the shape of the problem: **every buyer of one SKU contends on
one row.** `UPDATE products SET stock = stock - $2 WHERE sku = $1 AND stock >= $2`
takes a row lock that is held until the transaction commits, so the product row
serialises the entire sale. Throughput becomes a function of transaction latency,
and 50 concurrent buyers spend most of their time queued behind that lock — which
is exactly what the 53 ms median says.

The ramp added the part this section originally got wrong. Serialisation is not a
tax paid on every request; it is a cliff. Up to 700 req/s the lock is free when a
request arrives and v1 answers in 1.4 ms — better than v2 does at v2's own
sustainable rate. The interesting quantity was never v1's latency, it was the
arrival rate at which the lock stops keeping up, and past that point the queue
does the rest.

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

50 VUs is very likely not enough to saturate v2 or v3 — p99 stayed in the single
digits of milliseconds throughout, which is not the signature of a system under
pressure. Every Redis-path figure here is a floor, not a ceiling; Phase 5's
ramping profile is what will find the knee.
