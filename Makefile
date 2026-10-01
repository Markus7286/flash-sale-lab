.PHONY: up down logs test bench bench-v0 bench-v0b bench-v1 bench-v2 bench-v3 correctness ramp spike soak fault-test psql redis-cli

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

# The tests skip themselves rather than fail when Postgres or Redis is unreachable.
test:
	go test ./... -count=1

# v0 and v0b live in the Laravel server; every other version in the Go api. The
# scripts are identical either way: only BASE_URL changes.
api_url = $(if $(filter v0 v0b,$(1)),http://api-php:8090,http://api:8080)

K6 = docker run --rm -i \
	--network $$(docker compose ps --format '{{.Name}}' api | head -1 | sed 's/-api-1/_default/') \
	-v "$$(pwd)/k6:/scripts" \
	grafana/k6:latest run

# Every knob the scripts read from __ENV. docker run does not inherit make's
# variables, so without this `make soak DURATION=30m` silently ran for 10 minutes.
K6_VARS = STOCK VUS DURATION STEP STEPS STEP_SECONDS MAX_VUS CALM PEAK CALM_SECONDS PEAK_SECONDS RATE SKU PER_USER_LIMIT
k6_env = $(foreach v,$(K6_VARS),$(if $($(v)),-e $(v)=$($(v))))

# $(call k6,script,version)
k6 = $(K6) /scripts/$(1) -e VERSION=$(2) -e BASE_URL=$(call api_url,$(2)) $(k6_env)

# Laravel, raw queries through DB::selectOne(); the same transaction as v1.
bench-v0:
	$(call k6,flash-sale.js,v0)

# Laravel, the same transaction through Eloquent models.
bench-v0b:
	$(call k6,flash-sale.js,v0b)

# The Postgres conditional UPDATE path.
bench-v1:
	$(call k6,flash-sale.js,v1)

# The Redis Lua path.
bench-v2:
	$(call k6,flash-sale.js,v2)

# The Redis Lua path plus a stream publish, persisted by the worker.
bench-v3:
	$(call k6,flash-sale.js,v3)

bench: bench-v0 bench-v0b bench-v1 bench-v2 bench-v3

# Open-model profiles. These drive arrival rate rather than VU count, so they can
# actually overload the service -- a constant-VU test never can.
VERSION ?= v2

# Steps the arrival rate up to find the knee.
ramp:
	$(call k6,ramp.js,$(VERSION))

# Quiet, then an instant jump to PEAK, then quiet again.
spike:
	$(call k6,spike.js,$(VERSION))

# 10 minutes below the knee, DURATION=30m for longer. Watch Grafana, not the
# summary: a soak fails on drift, and an aggregate is exactly what hides drift.
soak:
	$(call k6,soak.js,$(VERSION))

# Exact-count correctness over HTTP: 1,000 buyers for 100 units, concurrent replays,
# the per-user limit. Not a benchmark; it is what CI runs against every version.
correctness: correctness-v1 correctness-v2 correctness-v3 correctness-v0 correctness-v0b

correctness-%:
	$(call k6,correctness.js,$*) --quiet

# Kills the worker and every Postgres connection during bench-v3, then checks the books.
fault-test:
	./scripts/fault-test.sh

psql:
	docker compose exec postgres psql -U flashsale -d flashsale

redis-cli:
	docker compose exec redis redis-cli
