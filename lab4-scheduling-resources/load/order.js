// Нагрузка на путь заказа (api -> postgres) для krr (часть 3) и SLA (часть 8).
//
//   k6 run load/order.js                                    # профиль ~40 мин, BASE_URL по умолчанию
//   BASE_URL=http://k3s-w2.lan:32040 PEAK_RPS=300 k6 run load/order.js
//   DURATION_SCALE=0.1 k6 run load/order.js                 # быстрый прогон профиля (~4 мин)
//
// Профиль с открытой моделью (arrival rate): k6 держит заданный RPS независимо от того,
// как быстро отвечает сервис, поэтому деградация видна как рост latency и ошибок,
// а не как «просевший» RPS. Есть спокойные участки и пики — для p95 и max у krr.
import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://k3s-w1.lan:32040';
const PEAK = Number(__ENV.PEAK_RPS || 200);
const SCALE = Number(__ENV.DURATION_SCALE || 1);
const m = (min) => `${Math.max(1, Math.round(min * 60 * SCALE))}s`;

export const options = {
  scenarios: {
    orders: {
      executor: 'ramping-arrival-rate',
      startRate: 10,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 500,
      stages: [
        { target: Math.round(PEAK * 0.25), duration: m(3) },  // разгон
        { target: Math.round(PEAK * 0.25), duration: m(10) }, // спокойный фон
        { target: PEAK, duration: m(2) },                     // подъём к пику
        { target: PEAK, duration: m(8) },                     // пик
        { target: Math.round(PEAK * 0.1), duration: m(2) },   // спад
        { target: Math.round(PEAK * 0.1), duration: m(5) },   // почти простой
        { target: PEAK, duration: m(2) },                     // второй пик
        { target: PEAK, duration: m(6) },
        { target: 0, duration: m(2) },
      ],
    },
  },
  thresholds: {
    'http_req_failed{name:POST /order}': ['rate<0.01'],
    'http_req_duration{name:POST /order}': ['p(95)<300'],
  },
};

const params = { headers: { 'Content-Type': 'application/json' } };

export default function () {
  // 90% — создание заказа (критический путь), 10% — чтение списка.
  if (Math.random() < 0.9) {
    const body = JSON.stringify({ item: `k6-${__VU}`, quantity: 1 + (__ITER % 5) });
    const res = http.post(`${BASE_URL}/order`, body, { ...params, tags: { name: 'POST /order' } });
    check(res, { 'order created': (r) => r.status >= 200 && r.status < 300 });
  } else {
    const res = http.get(`${BASE_URL}/orders?limit=10`, { tags: { name: 'GET /orders' } });
    check(res, { 'orders listed': (r) => r.status === 200 });
  }
}
