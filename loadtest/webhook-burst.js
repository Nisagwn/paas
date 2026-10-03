// Push burst: many signed GitHub push webhooks at once, then wait until every
// deployment is ready. Measures how fast the control plane accepts pushes
// and how long the queue + worker pipeline takes to drain them.
//
//   k6 run -e PAAS_URL=http://localhost:8080 -e TOKEN=... -e SECRET=... \
//          -e APP=loadtest -e REPO=nisagwn/loadtest -e PUSHES=50 loadtest/webhook-burst.js
import http from 'k6/http';
import crypto from 'k6/crypto';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const BASE = __ENV.PAAS_URL || 'http://localhost:8080';
const TOKEN = __ENV.TOKEN;
const SECRET = __ENV.SECRET;
const APP = __ENV.APP || 'loadtest';
const REPO = __ENV.REPO || 'nisagwn/loadtest';
const PUSHES = parseInt(__ENV.PUSHES || '50', 10);
const VUS = parseInt(__ENV.VUS || '10', 10);
const READY_TIMEOUT_S = parseInt(__ENV.READY_TIMEOUT || '600', 10);

const enqueueLatency = new Trend('paas_enqueue_ms', true);
const timeToReady = new Trend('paas_time_to_ready_ms', true);
const failedDeploys = new Counter('paas_failed_deployments');

export const options = {
  scenarios: {
    burst: { executor: 'shared-iterations', vus: VUS, iterations: PUSHES, maxDuration: '15m' },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    paas_enqueue_ms: ['p(95)<500'],
  },
};

const auth = { headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' } };

export function setup() {
  if (!TOKEN || !SECRET) throw new Error('set -e TOKEN=<PAAS_API_TOKEN> -e SECRET=<PAAS_GITHUB_WEBHOOK_SECRET>');
  // Create the app; 409 means it already exists.
  const res = http.post(`${BASE}/api/apps`, JSON.stringify({ name: APP, repo: REPO }), auth);
  check(res, { 'app ready': (r) => r.status === 201 || r.status === 409 });
}

function randomSHA() {
  let s = '';
  for (let i = 0; i < 40; i++) s += '0123456789abcdef'[Math.floor(Math.random() * 16)];
  return s;
}

export default function () {
  const sha = randomSHA();
  // Spread pushes over a few branches, like a team working in parallel.
  const branch = `load-${__ITER % 5}`;
  const body = JSON.stringify({
    ref: `refs/heads/${branch}`,
    after: sha,
    repository: { full_name: REPO },
    head_commit: { message: `k6 push ${__ITER}` },
  });
  const sig = 'sha256=' + crypto.hmac('sha256', SECRET, body, 'hex');

  const started = Date.now();
  const res = http.post(`${BASE}/webhooks/github`, body, {
    headers: { 'Content-Type': 'application/json', 'X-GitHub-Event': 'push', 'X-Hub-Signature-256': sig },
  });
  enqueueLatency.add(res.timings.duration);
  if (!check(res, { 'push accepted (201)': (r) => r.status === 201 })) return;
  const id = res.json('id');

  // Poll until the deployment reaches a final status.
  const deadline = started + READY_TIMEOUT_S * 1000;
  while (Date.now() < deadline) {
    const d = http.get(`${BASE}/api/deployments/${id}`, auth).json();
    if (d.status === 'ready') {
      timeToReady.add(Date.now() - started);
      return;
    }
    if (d.status === 'failed') {
      failedDeploys.add(1);
      return;
    }
    sleep(0.5);
  }
  failedDeploys.add(1);
}
