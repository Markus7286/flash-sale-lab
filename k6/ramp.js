// Finds the knee. Steps the arrival rate up in equal jumps and reports each step
// separately, because the interesting number is not the peak throughput -- it is
// the rate at which latency leaves the flat part of the curve.
//
// Arrival rate, not VUs: a constant-VU test cannot overload the system, since
// slower responses simply mean fewer requests. Open-model load keeps arriving
// whether or not the previous one came back, which is what a real sale does.

import { commonOptions, seedStock, reserve, checkBooks } from './lib/sale.js';
import { scenarioThresholds, scenarioTable, overall } from './lib/report.js';

const STOCK = Number(__ENV.STOCK || 20000000);
const STEP = Number(__ENV.STEP || 2000);
const STEPS = Number(__ENV.STEPS || 10);
const STEP_SECONDS = Number(__ENV.STEP_SECONDS || 20);
// Headroom for the step where responses slow down and arrivals keep coming.
const MAX_VUS = Number(__ENV.MAX_VUS || 1500);

const steps = [];
for (let i = 1; i <= STEPS; i++) {
  const target = STEP * i;
  steps.push({ scenario: `step_${target}`, label: `step ${i}`, target, seconds: STEP_SECONDS });
}

const scenarios = {};
steps.forEach((step, i) => {
  scenarios[step.scenario] = {
    executor: 'constant-arrival-rate',
    rate: step.target,
    timeUnit: '1s',
    duration: `${STEP_SECONDS}s`,
    startTime: `${i * STEP_SECONDS}s`,
    preAllocatedVUs: Math.min(MAX_VUS, Math.max(50, Math.ceil(step.target / 100))),
    maxVUs: MAX_VUS,
  };
});

export const options = {
  ...commonOptions,
  scenarios,
  // No pass/fail bound on latency here on purpose: a step that degrades is the
  // result, not a failure. Only a broken request counts as broken.
  thresholds: { ...scenarioThresholds(steps.map((s) => s.scenario)), http_req_failed: ['rate<0.01'] },
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
      `RAMP  ${STEPS} steps of ${STEP}/s, ${STEP_SECONDS}s each`,
      '',
      scenarioTable(data, steps, { targetHeader: 'arrival' }),
      '',
      overall(data),
      '',
      'The knee is the first step where achieved falls short of arrival, or where',
      'p99 leaves the flat part of the column above it.',
      '',
    ].join('\n'),
  };
}
