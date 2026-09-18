#!/usr/bin/env bash
# Kill the worker and every Postgres connection while bench-v3 runs, then prove
# no order was lost or duplicated once the backlog drains.
set -euo pipefail

SKU=bench-v3
DB_USER=${POSTGRES_USER:-flashsale}
DB_NAME=${POSTGRES_DB:-flashsale}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-300}

rcli() { docker compose exec -T redis redis-cli --raw "$@"; }
sql() { docker compose exec -T postgres psql -U "$DB_USER" -d "$DB_NAME" -tAc "$1"; }

make bench-v3 &
bench=$!

sleep 10
echo "fault: killing worker"
docker compose kill worker
sleep 5
echo "fault: starting worker"
docker compose start worker

sleep 5
echo "fault: terminating postgres connections"
sql "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
      WHERE datname = '$DB_NAME' AND pid <> pg_backend_pid()"

wait "$bench"

# XLEN counts pending entries too, so zero means every order was settled; the
# killed worker's entries need STREAM_CLAIM_IDLE before anyone reclaims them.
echo "waiting up to ${DRAIN_TIMEOUT}s for the backlog to drain"
drained=0
for _ in $(seq 1 "$DRAIN_TIMEOUT"); do
  if [ "$(rcli XLEN "{$SKU}:orders")" = "0" ]; then
    drained=1
    break
  fi
  sleep 1
done

total=$(rcli GET "{$SKU}:total")
remaining=$(rcli GET "{$SKU}:stock")
sold=$(rcli GET "{$SKU}:sold")
queued=$(rcli GET "{$SKU}:queued")
request_ids=$(rcli SCARD "{$SKU}:reqs")
dead=$(rcli XLEN "{$SKU}:dead")
persisted=$(sql "SELECT COALESCE(sum(qty), 0) FROM sale_orders WHERE sku = '$SKU'")
rows=$(sql "SELECT count(*) FROM sale_orders WHERE sku = '$SKU'")

fail=0
check() {
  if [ "$2" = "$3" ]; then
    echo "ok   $1 ($2)"
  else
    echo "FAIL $1: got $2, want $3"
    fail=1
  fi
}

check "backlog drained" "$drained" 1
check "remaining + sold == total" "$((remaining + sold))" "$total"
check "persisted == sold" "$persisted" "$sold"
check "queued == 0" "${queued:-0}" 0
check "order rows == request_ids (no loss, no duplicate)" "$rows" "$request_ids"
echo "dead letters: $dead"

exit "$fail"
