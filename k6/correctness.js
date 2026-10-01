// The Go correctness suite, over HTTP, so it can be pointed at any server that
// claims the contract -- in particular the Laravel one, which the Go tests cannot
// reach. Every assertion is on an exact count, never a rate or a tolerance.
//
//   concurrent  stock 100, 1,000 buyers at once -> exactly 100 reserved, 900 sold out
//   replay      200 callers sharing one request_id -> exactly one unit taken
//   limit       one user, 20 concurrent attempts -> exactly PER_USER_LIMIT units
//
// This is not a benchmark: it measures nothing and is safe to run in CI.

import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Rate } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const VERSION = __ENV.VERSION || 'v1';
const PREFIX = `correctness-${VERSION}`;
// Must match the server's PER_USER_LIMIT; compose defaults both to 1.
const PER_USER_LIMIT = Number(__ENV.PER_USER_LIMIT || 1);

const STOCK = 100;
const BUYERS = 1000;
const REPLAYS = 200;
const LIMIT_ATTEMPTS = 20;

const reserved = new Counter('reserved');
const duplicate = new Counter('duplicate');
const soldOut = new Counter('sold_out');
const userLimit = new Counter('user_limit');
// Fed on every request, so the threshold below always has samples to judge.
const unexpected = new Rate('unexpected_outcome');

http.setResponseCallback(http.expectedStatuses(200, 201, 409));

const once = (vus) => ({ executor: 'per-vu-iterations', vus, iterations: 1, maxDuration: '60s' });

export const options = {
  scenarios: {
    concurrent: { ...once(BUYERS), exec: 'concurrent' },
    replay: { ...once(REPLAYS), exec: 'replay' },
    limit: { ...once(LIMIT_ATTEMPTS), exec: 'limit' },
  },
  thresholds: {
    'reserved{scenario:concurrent}': [`count==${STOCK}`],
    'sold_out{scenario:concurrent}': [`count==${BUYERS - STOCK}`],
    'reserved{scenario:replay}': ['count==1'],
    'duplicate{scenario:replay}': [`count==${REPLAYS - 1}`],
    'reserved{scenario:limit}': [`count==${PER_USER_LIMIT}`],
    'user_limit{scenario:limit}': [`count==${LIMIT_ATTEMPTS - PER_USER_LIMIT}`],
    unexpected_outcome: ['rate==0'],
  },
};

const sku = (name) => `${PREFIX}-${name}`;

function seed(name) {
  const res = http.put(`${BASE_URL}/${VERSION}/admin/skus/${sku(name)}/stock`, JSON.stringify({ stock: STOCK }), {
    headers: { 'Content-Type': 'application/json' },
  });
  if (res.status !== 200) {
    throw new Error(`seed ${sku(name)} failed: ${res.status} ${res.body}`);
  }
}

function stats(name) {
  const res = http.get(`${BASE_URL}/${VERSION}/skus/${sku(name)}/stock`);
  if (res.status !== 200) {
    throw new Error(`stats ${sku(name)} failed: ${res.status} ${res.body}`);
  }
  return res.json();
}

// allowed: the outcomes this scenario may legitimately produce.
function reserve(name, userId, requestId, allowed) {
  const res = http.post(`${BASE_URL}/${VERSION}/flash-sale`, JSON.stringify({ sku: sku(name), qty: 1 }), {
    headers: { 'Content-Type': 'application/json', 'X-User-Id': userId, 'X-Request-Id': requestId },
  });
  const status = res.status < 500 ? (res.json() || {}).status : undefined;
  const counters = { reserved, duplicate, sold_out: soldOut, user_limit: userLimit };

  if (allowed.includes(status)) {
    counters[status].add(1);
    unexpected.add(false);
    return;
  }
  unexpected.add(true);
  console.error(`${exec.scenario.name}: unexpected ${res.status} ${res.body}`);
}

export function setup() {
  seed('concurrent');
  seed('replay');
  seed('limit');

  const res = http.post(`${BASE_URL}/${VERSION}/flash-sale`, JSON.stringify({ sku: sku(`unknown-${Date.now()}`) }), {
    headers: { 'Content-Type': 'application/json', 'X-User-Id': 'nobody', 'X-Request-Id': `unknown-${Date.now()}` },
  });
  if (res.status !== 404 || res.json().status !== 'unknown_sku') {
    throw new Error(`an unseeded sku must be refused, got ${res.status} ${res.body}`);
  }
}

export function concurrent() {
  const id = exec.vu.idInTest;
  reserve('concurrent', `buyer-${id}`, `concurrent-${id}`, ['reserved', 'sold_out']);
}

export function replay() {
  reserve('replay', `replayer-${exec.vu.idInTest}`, 'the-one-request', ['reserved', 'duplicate']);
}

export function limit() {
  reserve('limit', 'the-one-user', `limit-${exec.vu.idInTest}`, ['reserved', 'user_limit']);
}

// The server's own books must agree with what the clients were told.
export function teardown() {
  const expect = (name, want) => {
    const got = stats(name);
    for (const [field, value] of Object.entries(want)) {
      if (got[field] !== value) {
        throw new Error(`${sku(name)}: ${field}=${got[field]}, want ${value} (${JSON.stringify(got)})`);
      }
    }
    console.log(`ok ${sku(name)} ${JSON.stringify(got)}`);
  };

  expect('concurrent', { remaining: 0, units_sold: STOCK, reservations: STOCK, buyers: STOCK });
  expect('replay', { remaining: STOCK - 1, units_sold: 1, reservations: 1, buyers: 1 });
  expect('limit', {
    remaining: STOCK - PER_USER_LIMIT,
    units_sold: PER_USER_LIMIT,
    reservations: PER_USER_LIMIT,
    buyers: 1,
  });
}
