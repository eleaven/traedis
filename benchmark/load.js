// Load test through Traefik, run by run.sh with the k6 service of compose.yml.
// For each sample backend, three cases run one after the other:
//   direct: the route without the cache middleware (the reference);
//   hit:    the cached route, one URL already stored;
//   miss:   the cached route, a new URL for every request.
// The result is printed as JSON on stdout; the run fails when the responses do
// not carry the Cache-Status their case is named after.
import http from 'k6/http';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://ingress';
const VUS = Number(__ENV.LOAD_VUS || 10);
const SECONDS = Number(__ENV.LOAD_SECONDS || 10);
const GAP = 3; // seconds between two cases, for the last requests to finish

const BACKENDS = {
  // Tiny text without Cache-Control (defaultTtl); wait makes it a slow service.
  whoami: { direct: '/whoami', cached: '/whoami-cache', params: ['wait=100ms'] },
  // Rendered PNG, with max-age.
  placeholder: { direct: '/placeholder/600x400', cached: '/placeholder-cache/600x400', params: [] },
  // imgproxy converting a placeholder image to AVIF, with max-age: a costly service (it ignores the query).
  imaging: {
    direct: '/imaging/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif',
    cached: '/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif',
    params: [],
  },
};
const CASES = ['direct', 'hit', 'miss'];

const scenarios = {};
const metrics = {};
const thresholds = {};
for (const backend of Object.keys(BACKENDS)) {
  for (const name of CASES) {
    const id = `${backend}_${name}`;
    scenarios[id] = {
      executor: 'constant-vus',
      vus: VUS,
      duration: `${SECONDS}s`,
      startTime: `${Object.keys(scenarios).length * (SECONDS + GAP)}s`,
      gracefulStop: `${GAP}s`,
      env: { BACKEND: backend, CASE: name },
    };
    metrics[id] = { duration: new Trend(`${id}_duration`, true), expected: new Rate(`${id}_expected`) };
    thresholds[`${id}_expected`] = ['rate>0.99'];
  }
}

export const options = {
  scenarios,
  thresholds,
  summaryTrendStats: ['count', 'avg', 'med', 'p(95)'],
};

// target returns the URL of a route of a backend: its own parameters, then params.
function target(routes, route, ...params) {
  const query = routes.params.concat(params).join('&');
  return `${BASE_URL}${routes[route]}${query ? '?' + query : ''}`;
}

// setup waits for the routes, stores the "hit" URLs and returns the run id that
// keeps the URLs of this run apart from those of previous runs.
export function setup() {
  const run = Date.now().toString(36);
  for (const routes of Object.values(BACKENDS)) {
    for (let i = 0; http.get(target(routes, 'cached', `run=${run}`)).status !== 200; i++) {
      if (i === 30) {
        exec.test.abort(`${routes.cached} is not ready`);
      }
      sleep(1);
    }
  }
  return { run };
}

export default function (data) {
  const routes = BACKENDS[__ENV.BACKEND];
  let url = target(routes, 'direct', `run=${data.run}`);
  let wanted = ''; // no Cache-Status: the plugin is not on the route
  if (__ENV.CASE === 'hit') {
    url = target(routes, 'cached', `run=${data.run}`);
    wanted = 'traedis; hit';
  } else if (__ENV.CASE === 'miss') {
    url = target(routes, 'cached', `run=${data.run}`, `i=${exec.vu.idInTest}-${exec.vu.iterationInScenario}`);
    wanted = 'traedis; fwd=uri-miss';
  }

  const res = http.get(url);
  const status = res.headers['Cache-Status'] || '';
  const metric = metrics[exec.scenario.name];
  metric.duration.add(res.timings.duration);
  metric.expected.add(res.status === 200 && (wanted === '' ? status === '' : status.includes(wanted)));
}

function round(n) {
  return Math.round(n * 100) / 100;
}

export function handleSummary(data) {
  const out = { vus: VUS, seconds: SECONDS };
  for (const backend of Object.keys(BACKENDS)) {
    out[backend] = {};
    for (const name of CASES) {
      const duration = data.metrics[`${backend}_${name}_duration`].values;
      out[backend][name] = {
        url: target(BACKENDS[backend], name === 'direct' ? 'direct' : 'cached').replace(BASE_URL, ''),
        requests: duration.count,
        requests_per_second: round(duration.count / SECONDS),
        avg_ms: round(duration.avg),
        p50_ms: round(duration.med),
        p95_ms: round(duration['p(95)']),
        expected_responses: round(data.metrics[`${backend}_${name}_expected`].values.rate),
      };
      if (name !== 'direct') {
        // The gain of the cache: above 1, the cached route serves more requests.
        out[backend][name].throughput_vs_direct = round(duration.count / out[backend].direct.requests);
      }
    }
  }
  return { stdout: JSON.stringify(out, null, 2) + '\n' };
}
