// Request throughput against a deployed app, through the full path:
// DNS → Traefik (TLS) → Service → pod. Run it against the production alias
// and against a deployment URL to compare, or during a rollback to check
// that no request fails while the alias moves.
//
//   k6 run -e URL=https://blog.paas.example.com/ -e RATE=200 -e DURATION=60s loadtest/app-traffic.js
import http from 'k6/http';
import { check } from 'k6';

const URL = __ENV.URL;
const RATE = parseInt(__ENV.RATE || '100', 10);

export const options = {
  scenarios: {
    constant: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '60s',
      preAllocatedVUs: Math.max(10, Math.ceil(RATE / 4)),
      maxVUs: RATE * 2,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(95)<300'],
  },
};

export default function () {
  if (!URL) throw new Error('set -e URL=https://<app>.<domain>/');
  const res = http.get(URL);
  check(res, { '200': (r) => r.status === 200 });
}
