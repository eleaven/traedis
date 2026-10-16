// Prints benchmark/results.json (written by benchmark/run.sh) as markdown tables, for the summary of the bench job
// of ci.yml:
//   node .github/scripts/bench-summary.mjs >> "$GITHUB_STEP_SUMMARY"
import { readFileSync } from 'node:fs';

const CASES = { direct: 'Without cache', hit: 'Cache hit', miss: 'Cache miss' };

// [label, 'num' for a right-aligned column]
const LOAD_COLUMNS = [
  ['Backend'], ['Case'], ['URL'], ['Requests/s', 'num'], ['p50', 'num'], ['p95', 'num'], ['vs without cache', 'num'],
];
const YAEGI_COLUMNS = [
  ['Benchmark'], ['Time per request', 'num'], ['Memory per request', 'num'], ['Allocations per request', 'num'],
];

const YAEGI_NOTE = 'The middleware called in-process, against a real Redis: cost of one request.';

function loadNote(k6) {
  return `k6, ${k6.vus} virtual users for ${k6.seconds} s per case. ` +
    'Each backend is requested on its route without the cache, then on the cached route: ' +
    'one stored URL (hit), and a new URL for every request (miss).';
}

// backends lists the backends of the k6 results (its other keys are the run's settings).
function backends(k6) {
  return Object.keys(k6).filter((key) => typeof k6[key] === 'object');
}

function number(value, digits) {
  return value.toLocaleString('en-US', { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

function milliseconds(ms) {
  return number(ms, ms < 10 ? 2 : ms < 100 ? 1 : 0) + ' ms';
}

function duration(ns) {
  if (ns < 1e6) {
    return number(ns / 1e3, ns < 1e5 ? 1 : 0) + ' µs';
  }
  return milliseconds(ns / 1e6);
}

function bytes(n) {
  if (n < 1 << 20) {
    return number(n / 1024, n < 100 << 10 ? 1 : 0) + ' KiB';
  }
  return number(n / (1 << 20), 2) + ' MiB';
}

function rate(perSecond) {
  return number(perSecond, perSecond < 100 ? 1 : 0);
}

// gain formats throughput_vs_direct, absent from the case without the cache.
function gain(ratio) {
  return ratio === undefined ? '—' : '×' + number(ratio, ratio < 10 ? 2 : 1);
}

// warning is not empty when some responses did not carry the expected Cache-Status.
function warning(result) {
  return result.expected_responses < 1 ? '⚠ ' + number(100 * result.expected_responses, 0) + '% expected responses' : '';
}

const raw = readFileSync(new URL('../../benchmark/results.json', import.meta.url), 'utf8');
const results = JSON.parse(raw);

function table(columns, rows) {
  const line = (cells) => `| ${cells.join(' | ')} |`;
  return [
    line(columns.map(([label]) => label)),
    line(columns.map(([, className]) => (className === 'num' ? '--:' : '---'))),
    ...rows.map(line),
  ].join('\n');
}

const load = [];
for (const backend of backends(results.k6)) {
  Object.entries(results.k6[backend]).forEach(([name, result], i) => {
    const alert = warning(result);
    load.push([
      i === 0 ? `**${backend}**` : '',
      CASES[name] || name,
      `\`${result.url}\``,
      rate(result.requests_per_second),
      milliseconds(result.p50_ms),
      milliseconds(result.p95_ms),
      (result.throughput_vs_direct === undefined ? gain() : `**${gain(result.throughput_vs_direct)}**`) +
        (alert ? `<br>${alert}` : ''),
    ]);
  });
}

const yaegi = Object.entries(results.yaegi).map(([name, result]) => [
  `\`${name}\``,
  duration(result.ns_per_op),
  bytes(result.bytes_per_op),
  number(result.allocs_per_op, 0),
]);

console.log(`## Traedis benchmarks

### Load test through Traefik

${loadNote(results.k6)}

${table(LOAD_COLUMNS, load)}

### Handler under Yaegi

${YAEGI_NOTE}

${table(YAEGI_COLUMNS, yaegi)}

<details>
<summary>results.json</summary>

\`\`\`json
${raw.trimEnd()}
\`\`\`

</details>`);
