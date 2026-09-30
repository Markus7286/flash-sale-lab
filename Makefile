.PHONY: up down logs test bench bench-v1 bench-v2 bench-v3 ramp spike soak fault-test psql redis-cli

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

# The tests skip themselves rather than fail when Postgres or Redis is unreachable.
test:
	go test ./... -count=1

K6 = docker run --rm -i \
	--network $$(docker compose ps --format '{{.Name}}' api | head -1 | sed 's/-api-1/_default/') \
	-v "$$(pwd)/k6:/scripts" -e BASE_URL=http://api:8080 \
	grafana/k6:latest run

K6_RUN = $(K6) /scripts/flash-sale.js

# The Postgres conditional UPDATE path.
bench-v1:
	$(K6_RUN) -e VERSION=v1

# The Redis Lua path.
bench-v2:
	$(K6_RUN) -e VERSION=v2

# The Redis Lua path plus a stream publish, persisted by the worker.
bench-v3:
	$(K6_RUN) -e VERSION=v3

bench: bench-v1 bench-v2 bench-v3

# Open-model profiles. These drive arrival rate rather than VU count, so they can
# actually overload the service -- a constant-VU test never can.
VERSION ?= v2

# Steps the arrival rate up to find the knee.
ramp:
	$(K6) /scripts/ramp.js -e VERSION=$(VERSION)

# Quiet, then an instant jump to PEAK, then quiet again.
spike:
	$(K6) /scripts/spike.js -e VERSION=$(VERSION)

# 30 minutes below the knee. Watch Grafana, not the summary: a soak fails on
# drift, and an aggregate over 30 minutes is exactly what hides drift.
soak:
	$(K6) /scripts/soak.js -e VERSION=$(VERSION)

# Kills the worker and every Postgres connection during bench-v3, then checks the books.
fault-test:
	./scripts/fault-test.sh

psql:
	docker compose exec postgres psql -U flashsale -d flashsale

redis-cli:
	docker compose exec redis redis-cli
