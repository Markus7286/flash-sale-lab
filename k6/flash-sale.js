// Throughput baseline for one reservation backend: point VERSION at v1 or v2 and
// everything else stays identical, so the two numbers are comparable.
//
// Stock is seeded far above what a run can consume on purpose, because a sold-out
// request takes a shorter code path and letting the SKU run dry would flatter
// whichever backend rejects faster.

import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Rate } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const VERSION = __ENV.VERSION || 'v2';
const SKU = __ENV.SKU || `bench-${VERSION}`;
const STOCK = Number(__ENV.STOCK || 2000000);
const VUS = Number(__ENV.VUS || 50);
const DURATION = __ENV.DURATION || '30s';

const reserved = new Counter('flash_sale_reserved');
const soldOut = new Counter('flash_sale_sold_out');
const rejected = new Counter('flash_sale_rejected');
const failures = new Rate('flash_sale_failures');

export const options = {
  scenarios: {
    baseline: { executor: 'constant-vus', vus: VUS, duration: DURATION },
  },
  // 409 is a business answer, not a transport failure, so it must not pollute http_req_failed.
  responseCallback: http.expectedStatuses(200, 201, 409),
  thresholds: {
    flash_sale_failures: ['rate<0.01'],
    http_req_failed: ['rate<0.01'],
  },
  // k6 does not report p99 by default, and p99 is what the comparison table quotes.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const jsonHeaders = { 'Content-Type': 'application/json' };

export function setup() {
  const res = http.put(
    `${BASE_URL}/${VERSION}/admin/skus/${SKU}/stock`,
    JSON.stringify({ stock: STOCK }),
    { headers: jsonHeaders },
  );
  if (res.status !== 200) {
    throw new Error(`seed failed: ${res.status} ${res.body}`);
  }
  return { sku: SKU };
}

export default function (data) {
  // A repeated request_id would be answered from the idempotency check and never
  // touch the write path, so this must be unique across the whole run.
  const id = `${exec.vu.idInTest}-${exec.vu.iterationInInstance}`;

  const res = http.post(
    `${BASE_URL}/${VERSION}/flash-sale`,
    JSON.stringify({ sku: data.sku, qty: 1 }),
    {
      headers: {
        ...jsonHeaders,
        'X-User-Id': `bench-user-${id}`,
        'X-Request-Id': `bench-req-${id}`,
      },
    },
  );

  if (res.status === 201) {
    reserved.add(1);
    failures.add(false);
    return;
  }
  if (res.status === 409) {
    const status = (res.json() || {}).status;
    if (status === 'sold_out') {
      soldOut.add(1);
    } else {
      rejected.add(1);
    }
    failures.add(false);
    return;
  }

  rejected.add(1);
  failures.add(true);
}

// The books must balance here; zero-oversell itself is proven by the Go
// concurrency tests, not by this run.
export function teardown(data) {
  const res = http.get(`${BASE_URL}/${VERSION}/skus/${data.sku}/stock`);
  if (res.status !== 200) {
    throw new Error(`stats failed: ${res.status} ${res.body}`);
  }
  const stats = res.json();
  console.log(`[${VERSION}] ${JSON.stringify(stats)}`);

  if (stats.remaining < 0) {
    throw new Error(`oversold: remaining=${stats.remaining}`);
  }
  if (stats.remaining + stats.units_sold !== STOCK) {
    throw new Error(
      `books do not balance: remaining=${stats.remaining} + sold=${stats.units_sold} != ${STOCK}`,
    );
  }
}
