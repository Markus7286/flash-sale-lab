// Thirty minutes at a rate the system handles comfortably. A soak is not looking
// for a ceiling -- it is looking for the things a 30s run cannot show: a leaking
// connection pool, an unbounded stream, a consumer group whose pending list only
// grows, latency that drifts upward as Redis fills.
//
// The k6 summary here is an aggregate over the whole run, which by construction
// hides drift. The drift view is Grafana: flash_sale_stream_pending must come back
// to zero and flash_sale_request_duration_seconds must stay flat for the full
// half hour. This run exists to produce that half hour.

import { commonOptions, seedStock, reserve, checkBooks } from './lib/sale.js';
import { overall } from './lib/report.js';

const STOCK = Number(__ENV.STOCK || 30000000);
const RATE = Number(__ENV.RATE || 8000);
const DURATION = __ENV.DURATION || '30m';
const MAX_VUS = Number(__ENV.MAX_VUS || 1000);

export const options = {
  ...commonOptions,
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(50, Math.ceil(RATE / 100)),
      maxVUs: MAX_VUS,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    flash_sale_failures: ['rate<0.01'],
    // Generous on purpose: a soak fails on drift, and drift that matters shows up
    // as a p99 nowhere near the 8ms a short run gives.
    http_req_duration: ['p(99)<100'],
  },
};

export function setup() {
  return seedStock(STOCK);
}

export default function (data) {
  reserve(data);
}

export function teardown(data) {
  checkBooks(data);
}

export function handleSummary(data) {
  return {
    stdout: ['', `SOAK  ${RATE}/s for ${DURATION}`, '', overall(data), ''].join('\n'),
  };
}
