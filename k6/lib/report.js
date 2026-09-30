// Per-scenario numbers exist only as sub-metrics, and a sub-metric exists only if
// some threshold names it. So the step tables below are paid for with thresholds
// whose bounds are deliberately unreachable: they are there to create the
// sub-metric, not to pass or fail.

export function scenarioThresholds(names) {
  const thresholds = {};
  for (const name of names) {
    thresholds[`http_req_duration{scenario:${name}}`] = ['max>=0'];
    thresholds[`http_reqs{scenario:${name}}`] = ['count>=0'];
    thresholds[`flash_sale_failures{scenario:${name}}`] = ['rate>=0'];
  }
  return thresholds;
}

const pad = (s, n) => String(s).padEnd(n);
const padStart = (s, n) => String(s).padStart(n);

// rows: [{ scenario, label, seconds, target }]
export function scenarioTable(data, rows, { targetHeader = 'target' } = {}) {
  const head = ['', targetHeader, 'achieved', 'p50', 'p95', 'p99', 'max', 'fail'];
  const widths = [10, 11, 11, 9, 9, 9, 9, 7];
  const lines = [
    head.map((h, i) => (i === 0 ? pad(h, widths[i]) : padStart(h, widths[i]))).join(''),
    widths.map((w) => '-'.repeat(w)).join(''),
  ];

  for (const row of rows) {
    const dur = data.metrics[`http_req_duration{scenario:${row.scenario}}`];
    const reqs = data.metrics[`http_reqs{scenario:${row.scenario}}`];
    const fail = data.metrics[`flash_sale_failures{scenario:${row.scenario}}`];
    if (!dur || !reqs) continue;

    const achieved = reqs.values.count / row.seconds;
    const cells = [
      pad(row.label, widths[0]),
      padStart(row.target === undefined ? '-' : `${fmtInt(row.target)}/s`, widths[1]),
      padStart(`${fmtInt(achieved)}/s`, widths[2]),
      padStart(ms(dur.values.med), widths[3]),
      padStart(ms(dur.values['p(95)']), widths[4]),
      padStart(ms(dur.values['p(99)']), widths[5]),
      padStart(ms(dur.values.max), widths[6]),
      padStart(pct(fail ? fail.values.rate : 0), widths[7]),
    ];
    lines.push(cells.join(''));
  }
  return lines.join('\n');
}

export function overall(data) {
  const dur = data.metrics.http_req_duration.values;
  const reqs = data.metrics.http_reqs.values;
  const failed = data.metrics.http_req_failed.values;
  return [
    `requests      ${fmtInt(reqs.count)} (${fmtInt(reqs.rate)}/s over the whole run)`,
    `latency       p50 ${ms(dur.med)}  p95 ${ms(dur['p(95)'])}  p99 ${ms(dur['p(99)'])}  max ${ms(dur.max)}`,
    `http failures ${pct(failed.rate)}`,
  ].join('\n');
}

function fmtInt(n) {
  return Math.round(n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}
function ms(v) {
  if (v === undefined) return '-';
  return v >= 100 ? `${v.toFixed(0)}ms` : `${v.toFixed(2)}ms`;
}
function pct(rate) {
  return `${(rate * 100).toFixed(2)}%`;
}
