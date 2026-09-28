import http from 'k6/http';
import { Counter, Trend } from 'k6/metrics';
import { hmac } from 'k6/crypto';
import encoding from 'k6/encoding';

// 运行参数（全部可用环境变量覆盖）
const VUS = Number(__ENV.VUS || '64');
const HOLD = __ENV.HOLD || '10s';
const MODE = __ENV.MODE || 'seckill';
const ITEM_ID = Number(__ENV.ITEM || '16');

// NS = 本次运行的命名空间。默认取当前毫秒时间戳，保证每轮都不一样。
// 为什么必须唯一：Redis 里 seckill:item:*:user:* 长期存在、seckill:request:* 保留 24h，
// 复用同一批 user_id / request_id 会被幂等和限购逻辑正确拒绝，看起来就像"服务有 bug"。
const NS = Number(__ENV.NS || '0') || Date.now();
const RUN_BASE = NS * 1000;

const SECRET = 'shopping_market_secret';
// BASE 是压测目标地址，默认直连单体 8080。
// 阶段 15 把秒杀拆成独立进程之后，可以用它换目标做对照：
//   -e URL=http://127.0.0.1:8083   直压秒杀服务（不做鉴权代理，基线用这个）
//   -e URL=http://127.0.0.1:8081   走网关（多一跳，用来看网关的代价）
const BASE = __ENV.URL || 'http://127.0.0.1:8080';
const SECKILL_URL = `${BASE}/seckill/items/${ITEM_ID}/orders`;
const HEALTH_URL = `${BASE}/health`;

const cOk = new Counter('ok');
const cPerUser = new Counter('biz_per_user_limit');
const cStock = new Counter('biz_stock_insufficient');
const cDupReq = new Counter('biz_dup_request_id');
const cRateLimited = new Counter('biz_rate_limited');
const cOtherBiz = new Counter('biz_other');
const cConnErr = new Counter('conn_err');
const tErrSec = new Trend('conn_err_sec_into_test');

const T0 = Date.now();

export const options = {
  // 必须保留响应体：否则出错时拿不到错误信息，无法区分"限购/库存/限流/重复"
  discardResponseBodies: false,
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    bench: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '2s', target: VUS },
        { duration: HOLD, target: VUS },
      ],
      gracefulRampDown: '0s',
    },
  },
};

const B64_HEADER = encoding.b64encode(JSON.stringify({ alg: 'HS256', typ: 'JWT' }), 'rawurl');

function makeToken(userID) {
  const now = Math.floor(Date.now() / 1000);
  const payload = encoding.b64encode(JSON.stringify({ user_id: userID, iat: now, exp: now + 3600 }), 'rawurl');
  const data = `${B64_HEADER}.${payload}`;
  return `${data}.${hmac('sha256', SECRET, data, 'base64rawurl')}`;
}

function classifyBiz(status, body) {
  if (status === 429) { cRateLimited.add(1); return; }
  if (body.indexOf('\u8d85\u51fa\u6bcf\u4eba\u8d2d\u4e70\u9650\u5236') >= 0) { cPerUser.add(1); return; }
  if (body.indexOf('\u79d2\u6740\u5e93\u5b58\u4e0d\u8db3') >= 0) { cStock.add(1); return; }
  if (body.indexOf('request_id') >= 0) { cDupReq.add(1); return; }
  cOtherBiz.add(1);
}

export default function () {
  let res;
  if (MODE === 'health') {
    res = http.get(HEALTH_URL, { tags: { name: 'health' } });
  } else {
    // 用户与幂等键都带上 NS，保证跨轮、跨 VU、跨 ITER 三重唯一
    const seq = RUN_BASE + __VU * 100000 + __ITER;
    res = http.post(
      SECKILL_URL,
      JSON.stringify({ request_id: `n${NS}-${__VU}-${__ITER}`, quantity: 1 }),
      {
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${makeToken(seq)}`,
          'X-Forwarded-For': `18.${(__VU >> 8) % 250}.${__VU % 250}.${__ITER % 250}`,
        },
        tags: { name: 'seckill' },
      }
    );
  }
  if (res.status === 0) {
    cConnErr.add(1);
    tErrSec.add((Date.now() - T0) / 1000);
    return;
  }
  if (res.status === 200) { cOk.add(1); return; }
  classifyBiz(res.status, String(res.body || '').slice(0, 120));
}

export function handleSummary(data) {
  const v = (n) => (data.metrics[n] ? data.metrics[n].values.count : 0);
  const d = data.metrics.http_req_duration.values;
  const errSec = data.metrics.conn_err_sec_into_test;
  const errWin = errSec ? `connWin=[${errSec.values.min.toFixed(2)}s..${errSec.values.max.toFixed(2)}s]` : 'connWin=[none]';
  const total = v('ok') + v('biz_per_user_limit') + v('biz_stock_insufficient') + v('biz_dup_request_id') + v('biz_rate_limited') + v('biz_other') + v('conn_err');
  console.log(
    `RESULT ns=${NS} mode=${MODE} vus=${VUS} total=${total} ok=${v('ok')} rateLimited=${v('biz_rate_limited')} ` +
    `perUser=${v('biz_per_user_limit')} stock=${v('biz_stock_insufficient')} dupReq=${v('biz_dup_request_id')} ` +
    `otherBiz=${v('biz_other')} connErr=${v('conn_err')} ` +
    `avg=${d.avg.toFixed(1)}ms p50=${d['p(50)'].toFixed(1)}ms p95=${d['p(95)'].toFixed(1)}ms p99=${d['p(99)'].toFixed(1)}ms ${errWin}`
  );
  return {};
}
