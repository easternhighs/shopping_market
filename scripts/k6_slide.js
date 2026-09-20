import http from 'k6/http';
import { sleep } from 'k6';
import { Counter } from 'k6/metrics';
import { hmac } from 'k6/crypto';
import encoding from 'k6/encoding';

// 这个脚本专门验证「固定窗口 vs 滑动窗口」在窗口边界上的差别。
//
// 规则：同一个用户 1 秒内最多 100 次（服务端 rateLimitPerUser / rateLimitWindow）。
//
// 打法：同一个用户，分两批、每批 110 个请求（请求 id 各不相同）：
//   第一批打在某个自然秒的 500ms 处（离前后边界都很远）
//   第二批打在下个自然秒的 60ms 处
// 两批之间只隔约 510 毫秒。
//
// 固定窗口：两批落在不同的自然秒里，各自「没超 100」→ 两批各放行约 100 个，合计约 200。
//           但这 510 毫秒内真实放行量是 200 次，等于 1 秒上限被放大了接近 2 倍。
// 滑动窗口：第二批来时，第一批的 100 条记录还在「最近 1000 毫秒」里 → 第二批全被拒。

const BASE = __ENV.URL || 'http://127.0.0.1:8080';
const ITEM_ID = Number(__ENV.ITEM || '29');
// 每轮换一个用户，保证限流窗口是干净的
const USER_ID = Number(__ENV.USER || '0') || (900000000 + (Date.now() % 100000000));
const BATCH = Number(__ENV.BATCH || '110');

const SECRET = 'shopping_market_secret';
const SECKILL_URL = `${BASE}/seckill/items/${ITEM_ID}/orders`;

const cPass = new Counter('pass');
const cLimited = new Counter('rate_limited');

export const options = {
  discardResponseBodies: false,
  vus: 1,
  iterations: 1,
  summaryTrendStats: ['avg', 'p(95)'],
};

const B64_HEADER = encoding.b64encode(JSON.stringify({ alg: 'HS256', typ: 'JWT' }), 'rawurl');
function makeToken(userID) {
  const now = Math.floor(Date.now() / 1000);
  const payload = encoding.b64encode(JSON.stringify({ user_id: userID, iat: now, exp: now + 3600 }), 'rawurl');
  const data = `${B64_HEADER}.${payload}`;
  return `${data}.${hmac('sha256', SECRET, data, 'base64rawurl')}`;
}

const TOKEN = makeToken(USER_ID);
function buildBatch(tag) {
  const reqs = [];
  for (let i = 0; i < BATCH; i++) {
    reqs.push({
      method: 'POST',
      url: SECKILL_URL,
      body: JSON.stringify({ request_id: `slide-${USER_ID}-${tag}-${i}`, quantity: 1 }),
      params: {
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` },
        tags: { name: `batch_${tag}` },
      },
    });
  }
  return reqs;
}

function msIntoSecond() { return Date.now() % 1000; }

function countResults(tag, results) {
  let pass = 0, limited = 0;
  for (const r of results) {
    if (r.status === 429) limited++; else pass++;
  }
  cPass.add(pass); cLimited.add(limited);
  console.log(`批次 ${tag}: 放行(非429)=${pass} 限流(429)=${limited}`);
  return { pass, limited };
}

export default function () {
  // 对齐到「自然秒 + 500ms」
  sleep((((500 - msIntoSecond()) + 1000) % 1000) / 1000);
  const aStart = Date.now();
  const a = countResults('A', http.batch(buildBatch('A')));
  const aEnd = Date.now();

  // 再对齐到「下一个自然秒 + 60ms」
  sleep((((1060 - msIntoSecond()) + 1000) % 1000) / 1000);
  const bStart = Date.now();
  const b = countResults('B', http.batch(buildBatch('B')));
  const bEnd = Date.now();

  console.log(`A 时间窗: 秒内 ${aStart % 1000}~${aEnd % 1000}ms（耗时 ${aEnd - aStart}ms）`);
  console.log(`B 时间窗: 秒内 ${bStart % 1000}~${bEnd % 1000}ms（耗时 ${bEnd - bStart}ms）`);
  console.log(`两批间隔: ${bStart - aEnd}ms`);
  console.log(`RESULT user=${USER_ID} item=${ITEM_ID} 总计放行=${a.pass + b.pass} A放行=${a.pass} B放行=${b.pass}`);
}