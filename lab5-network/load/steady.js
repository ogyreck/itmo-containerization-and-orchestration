// Ровная нагрузка на путь заказа через NodePort — клиент для хаос-теста (часть 2).
// Открытая модель (constant-arrival-rate): k6 держит RATE запросов/с независимо от ответов,
// поэтому любой сбой виден как ошибка, а не как просевший RPS.
//
//   K6_PROMETHEUS_RW_SERVER_URL=http://k3s-mon.lan:32001/api/v1/write \
//   k6 run -o experimental-prometheus-rw --tag testid=chaos-1 load/steady.js
//
//   RATE=30 DURATION=15m BASE_URL=http://k3s-w2.lan:32040 k6 run ...
import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://k3s-w1.lan:32040';

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 20),
      timeUnit: '1s',
      duration: __ENV.DURATION || '10m',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  // Без ретраев: каждый неудачный запрос должен попасть в метрики
  thresholds: { http_req_failed: ['rate==0'] },
};

export default function () {
  const res = http.post(
    `${BASE_URL}/order`,
    JSON.stringify({ item: 'chaos', quantity: 1 }),
    // Таймаут короче дефолтных 60 с: запрос, ушедший в «мёртвый» IP, должен стать ошибкой, а не висеть
    { headers: { 'Content-Type': 'application/json' }, timeout: '3s', tags: { name: 'POST /order' } },
  );
  check(res, { 'order created': (r) => r.status === 201 });
}
