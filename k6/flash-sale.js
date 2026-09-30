// Throughput baseline for one reservation backend at fixed concurrency: point
// VERSION at v1, v2 or v3 and everything else stays identical, so the numbers are
// comparable. The load shape lives here; the request itself is in lib/sale.js.

import { commonOptions, seedStock, reserve, checkBooks } from './lib/sale.js';

const STOCK = Number(__ENV.STOCK || 2000000);
const VUS = Number(__ENV.VUS || 50);
const DURATION = __ENV.DURATION || '30s';

export const options = {
  ...commonOptions,
  scenarios: {
    baseline: { executor: 'constant-vus', vus: VUS, duration: DURATION },
  },
  thresholds: {
    flash_sale_failures: ['rate<0.01'],
    http_req_failed: ['rate<0.01'],
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
