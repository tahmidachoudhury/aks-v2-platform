// The P6 load test: run by make loadtest, never by Argo CD. See README.md.
//
// Two scenarios against the public host, through Traefik and api-gateway:
//   browse  RATE iterations a second of the dashboard page and a login. The
//           gateway doesn't rate limit either, so this is what drives its
//           CPU, and dashboard-api's, for the HPAs.
//   orders  One order a second, each publishing order.created for the
//           worker. One a second stays under the gateway's limit of 100 a
//           minute per client address, and each pod is its own address.
// Any failed request or check fails the thresholds, k6 exits non-zero and
// the Job fails.
import http from 'k6/http';
import { check, fail } from 'k6';

const BASE = __ENV.BASE_URL;
const RATE = parseInt(__ENV.RATE, 10);
const DURATION = __ENV.DURATION;
// Empty by default, so the host resolves to Traefik's public IP. Set to
// Traefik's ClusterIP when that path hides the pods' addresses (README.md).
const TARGET_IP = __ENV.TARGET_IP;
const HOST = BASE.replace(/^https:\/\//, '');

export const options = {
  hosts: TARGET_IP ? { [HOST]: TARGET_IP } : {},
  // The staging certificate isn't trusted. The test checks the services,
  // not the certificate; criterion 8 checks that from outside.
  insecureSkipTLSVerify: true,
  scenarios: {
    browse: {
      executor: 'constant-arrival-rate',
      exec: 'browse',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 10,
      maxVUs: 50,
    },
    orders: {
      executor: 'constant-arrival-rate',
      exec: 'orders',
      rate: 1,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 2,
      maxVUs: 10,
    },
  },
  thresholds: {
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
  },
};

const json = { headers: { 'Content-Type': 'application/json' } };

function login(email) {
  return http.post(`${BASE}/auth/login`, JSON.stringify({ email, password: 'loadtest' }), json);
}

export function setup() {
  const res = login('loadtest@example.com');
  if (res.status !== 200) fail(`login returned ${res.status}`);
  return { token: res.json('token') };
}

export function browse() {
  check(http.get(`${BASE}/`), { 'dashboard page 200': (r) => r.status === 200 });
  check(login('browse@example.com'), { 'login 200': (r) => r.status === 200 });
}

export function orders(data) {
  const res = http.post(
    `${BASE}/api/orders/`,
    JSON.stringify({ items: [{ product_id: 'loadtest', quantity: 1, price: 1.0 }], currency: 'GBP' }),
    { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${data.token}` } },
  );
  check(res, { 'order created': (r) => r.status === 201 });
}
