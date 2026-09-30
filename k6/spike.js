// A sale opening: quiet, then everyone at once, then quiet again. Three
// constant-arrival-rate scenarios back to back rather than a ramp, because the
// point is the instantaneous step -- no warm-up for the connection pool, no time
// for Redis to load the script, no gradual scaling of anything.
//
// What to read: the spike p99 against the calm p99 tells you the cost of the
// surge, and the recovery p99 tells you whether the system came back or stayed
// degraded.

import { commonOptions, seedStock, reserve, checkBooks } from './lib/sale.js';
import { scenarioThresholds, scenarioTable, overall } from './lib/report.js';

const STOCK = Number(__ENV.STOCK || 20000000);
const CALM = Number(__ENV.CALM || 500);
const PEAK = Number(__ENV.PEAK || 15000);
const CALM_SECONDS = Number(__ENV.CALM_SECONDS || 20);
const PEAK_SECONDS = Number(__ENV.PEAK_SECONDS || 30);
const MAX_VUS = Number(__ENV.MAX_VUS || 1500);

const phases = [
  { scenario: 'calm', label: 'calm', target: CALM, seconds: CALM_SECONDS, start: 0 },
  { scenario: 'spike', label: 'spike', target: PEAK, seconds: PEAK_SECONDS, start: CALM_SECONDS },
  {
    scenario: 'recover',
    label: 'recover',
    target: CALM,
    seconds: CALM_SECONDS,
    start: CALM_SECONDS + PEAK_SECONDS,
  },
];

const scenarios = {};
for (const phase of phases) {
  scenarios[phase.scenario] = {
    executor: 'constant-arrival-rate',
    rate: phase.target,
    timeUnit: '1s',
    duration: `${phase.seconds}s`,
    startTime: `${phase.start}s`,
    preAllocatedVUs: Math.min(MAX_VUS, Math.max(50, Math.ceil(phase.target / 100))),
    maxVUs: MAX_VUS,
  };
}

export const options = {
  ...commonOptions,
  scenarios,
  thresholds: {
    ...scenarioThresholds(phases.map((p) => p.scenario)),
    http_req_failed: ['rate<0.01'],
    flash_sale_failures: ['rate<0.01'],
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
    stdout: [
      '',
      `SPIKE  ${CALM}/s for ${CALM_SECONDS}s, then ${PEAK}/s for ${PEAK_SECONDS}s, then ${CALM}/s again`,
      '',
      scenarioTable(data, phases, { targetHeader: 'arrival' }),
      '',
      overall(data),
      '',
    ].join('\n'),
  };
}
