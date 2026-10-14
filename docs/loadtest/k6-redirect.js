// Нагрузочный тест редиректа GET /{code}.
//
// Запуск (сервер уже поднят, например через `make docker-up`):
//   k6 run -e BASE_URL=http://localhost:8080 docs/loadtest/k6-redirect.js
//
// setup() регистрирует отдельного пользователя и создаёт одну ссылку через API;
// основной сценарий многократно запрашивает /{code} без следования редиректу
// и проверяет статус 302.

import http from 'k6/http';
import { check, fail } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const API = `${BASE_URL}/api/v1`;
const JSON_HEADERS = { 'Content-Type': 'application/json' };

export const options = {
  scenarios: {
    redirect: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '30s', target: 50 },
        { duration: '1m', target: 200 },
        { duration: '2m', target: 200 },
        { duration: '30s', target: 0 },
      ],
      gracefulRampDown: '10s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<50'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export function setup() {
  const email = `loadtest-${Date.now()}-${Math.floor(Math.random() * 1e6)}@example.com`;
  const password = 'loadtest-password';

  const reg = http.post(
    `${API}/auth/register`,
    JSON.stringify({ email: email, password: password }),
    { headers: JSON_HEADERS },
  );
  if (reg.status !== 201) {
    fail(`register failed: status ${reg.status}, body ${reg.body}`);
  }
  const token = reg.json('token');

  const created = http.post(
    `${API}/links`,
    JSON.stringify({ url: 'https://example.com/loadtest' }),
    { headers: Object.assign({}, JSON_HEADERS, { Authorization: `Bearer ${token}` }) },
  );
  if (created.status !== 201) {
    fail(`create link failed: status ${created.status}, body ${created.body}`);
  }

  return { code: created.json('code') };
}

export default function (data) {
  const res = http.get(`${BASE_URL}/${data.code}`, { redirects: 0 });
  check(res, {
    'status is 302': (r) => r.status === 302,
  });
}
