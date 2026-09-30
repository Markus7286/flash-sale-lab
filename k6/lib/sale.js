// The reservation workload every profile in this directory shares. A ramp, a
// spike and a soak are only comparable to the baseline if they drive the exact
// same request, so the request lives here and the profiles only set the shape of
// the load.
//
// Stock is seeded far above what a run can consume on purpose, because a sold-out
// request takes a shorter code path and letting the SKU run dry would flatter
// whichever backend rejects faster.

import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Rate } from 'k6/metrics';

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
export const VERSION = __ENV.VERSION || 'v2';
export const SKU = __ENV.SKU || `bench-${VERSION}`;

export const reserved = new Counter('flash_sale_reserved');
export const soldOut = new Counter('flash_sale_sold_out');
export const rejected = new Counter('flash_sale_rejected');
export const failures = new Rate('flash_sale_failures');

// 409 is a business answer (sold out, over the per-user limit, replayed request),
// not a transport failure, so it must not pollute http_req_failed. This has to be
// a setResponseCallback call: k6 v2 rejects `responseCallback` as a script option
// and warns about an unknown field while silently counting every 409 as failed.
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

export const commonOptions = {
  // k6 does not report p99 by default, and p99 is what every table here quotes.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const jsonHeaders = { 'Content-Type': 'application/json' };

export function seedStock(stock) {
  const res = http.put(
    `${BASE_URL}/${VERSION}/admin/skus/${SKU}/stock`,
    JSON.stringify({ stock }),
    { headers: jsonHeaders },
  );
  if (res.status !== 200) {
    throw new Error(`seed failed: ${res.status} ${res.body}`);
  }
  return { sku: SKU, stock };
}

export function reserve(data) {
  // A repeated request_id is answered from the idempotency check and never
  // reaches the write path, so this has to be unique across the whole run --
  // including across scenarios, which is why the scenario name is in it.
  const id = `${exec.scenario.name}-${exec.vu.idInTest}-${exec.vu.iterationInScenario}`;

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
export function checkBooks(data) {
  const res = http.get(`${BASE_URL}/${VERSION}/skus/${data.sku}/stock`);
  if (res.status !== 200) {
    throw new Error(`stats failed: ${res.status} ${res.body}`);
  }
  const stats = res.json();
  console.log(`[${VERSION}] ${JSON.stringify(stats)}`);

  if (stats.remaining < 0) {
    throw new Error(`oversold: remaining=${stats.remaining}`);
  }
  if (stats.remaining + stats.units_sold !== data.stock) {
    throw new Error(
      `books do not balance: remaining=${stats.remaining} + sold=${stats.units_sold} != ${data.stock}`,
    );
  }
}
