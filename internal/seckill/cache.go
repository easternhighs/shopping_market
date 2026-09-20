package seckill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	redisclient "shopping_market/internal/pkg/redis"

	"github.com/redis/go-redis/v9"
)

// seckillInfoTTL 是秒杀商品信息缓存的有效期。
// 信息类数据（价格、限购、活动时间）改动不频繁，缓存一小段时间即可；
// 库存这种频繁变化的数据不用它缓存，走 :stock key。
const seckillInfoTTL = 5 * time.Minute

// requestIDTTLSeconds 是幂等键的存活时间：同一次抢购的 request_id 在这么长时间内只算一次。
const requestIDTTLSeconds = 86400

// rollbackLuaScript 用来把一次“Redis 已经预扣、但 MySQL 最终没落库”的请求还回去。
// KEYS[1]：Redis 剩余库存
// KEYS[2]：该用户在该商品上的已购买数量
// ARGV[1]：本次要回补的数量
const rollbackLuaScript = `
local stock = tonumber(redis.call("GET", KEYS[1]) or "0")
local bought = tonumber(redis.call("GET", KEYS[2]) or "0")
local quantity = tonumber(ARGV[1])

if bought < quantity then
	return -1 -- 用户已购买数量不足，不能回补
end

-- 为剩余库存加上quantity，为用户已购买数量减少quantity，当用户已购买数量不足时回补会失败
redis.call("INCRBY", KEYS[1], quantity)
redis.call("DECRBY", KEYS[2], quantity)
return 1 -- 回补成功
`

// hotPathLuaScript 是抢购接口的“热路径”：把一次抢购需要的全部 Redis 操作塞进同一个脚本，
// 于是整条链路在 Redis 上**只花一次网络往返**。
//
// 优化前这条链路要来回 4 次：
//  1. 限流计数 INCR
//  2. 读商品信息缓存 GET
//  3. 读活动信息缓存 GET
//  4. Lua 预扣 EVAL
//
// 每次往返都要等一个网络 RTT，4 次就是 4 个 RTT；隔离基准测出上限约 9,000/s，
// 合并成 1 次后约 27,000/s。这就是本脚本存在的理由。
//
// 参数约定：
//
//	KEYS[1] = 商品库存 key
//	KEYS[2] = 该用户在该商品上的已购买数量 key
//	KEYS[3] = requestID 幂等 key
//	KEYS[4] = 商品信息 key（JSON）
//	KEYS[5] = 商品所属活动的时间窗 key，格式 "开始时间戳:结束时间戳"（Unix 秒）
//	KEYS[6] = 限流 key（滑动窗口用的 ZSET：成员是 request_id，分数是放行时刻的毫秒时间戳）
//	ARGV[1] = quantity       本次购买数量
//	ARGV[2] = requestTTL     幂等 key 的过期秒数
//	ARGV[3] = now            当前 Unix 秒（由 Go 传入，避免脚本里的时间不确定）
//	ARGV[4] = rateLimitMax   任意一个窗口长度内最多放行多少次
//	ARGV[5] = rateWindowMs   限流窗口的毫秒数（也就是滑动窗口的长度）
//	ARGV[6] = nowMs          当前 Unix 毫秒（滑动窗口按毫秒比较，秒精度太粗）
//	ARGV[7] = requestID      本请求的幂等键，直接当作滑动窗口的成员
//
// 返回值约定（统一用「负数 = 被拒绝」表示）：
//
//	1   成功，脚本同时把商品 JSON 作为第二个元素带回来
//	-1  库存不足
//	-2  超过每人限购数量
//	-3  requestID 已经处理过（重复请求）
//	-4  被限流
//	-5  缓存还没准备好，调用方需要回源 MySQL 填充缓存后重试
//	-6  活动尚未开始
//	-7  活动已经结束
//
// 为什么先判活动时间再扣库存：时间校验是「这次请求到底成不成立」的前提。
// 如果先扣再判，活动没开始的时候就会先扣掉一份库存再想办法还回去，白白多一次操作。
const hotPathLuaScript = `
-- 1) 限流。滑动窗口：把每次「放行」的请求按毫秒时间戳记进一个 ZSET，
--    判断依据不是「现在是第几个固定窗口」，而是「最近一个窗口长度内放行过几次」。
--
--    固定窗口的毛病是跨边界能过两倍的量：限制 100 次/秒时，在第 1 秒的最后 10 毫秒
--    打 100 次、第 2 秒的头 10 毫秒再打 100 次，两次在各自的窗口里都「没超」，
--    但现实里这 20 毫秒内实际放过了 200 次。滑动窗口按「最近 1000 毫秒」算，
--    第二批会被直接拒掉。
--
--    放在最前面是有意的：被限流的请求连后面的库存都不碰，代价最小。
local nowMs = tonumber(ARGV[6])
local windowMs = tonumber(ARGV[5])
-- 先清掉已经滑出窗口的记录（分数 = 放行时刻的毫秒时间戳，所以下界可以是 0），
-- 再数窗口里还剩几条：这就是「最近一个窗口内放行过多少次」。
redis.call("ZREMRANGEBYSCORE", KEYS[6], 0, nowMs - windowMs)
local used = redis.call("ZCARD", KEYS[6])
if used >= tonumber(ARGV[4]) then
	return {-4}
end
-- 只有放行的请求才记进窗口。如果把被拒的也记上，窗口会被自己的拒绝记录越填越满，
-- 变成「越限封得越久」，那是另一种 bug。
-- 成员直接用 request_id：天然唯一，而且同一个请求重放（幂等键重复）不会多占配额。
redis.call("ZADD", KEYS[6], nowMs, ARGV[7])
-- 兜底过期：最后一个放行时刻之后再过整整一个窗口，这个 key 就可以整条消失了。
redis.call("PEXPIRE", KEYS[6], windowMs)

-- 2) 取商品信息和活动时间窗。缺任何一个都说明缓存没准备好；
--    这里不回源 MySQL（那会把数据库拖进高并发路径），而是让调用方走冷路径。
local itemJSON = redis.call("GET", KEYS[4])
local window = redis.call("GET", KEYS[5])
if not itemJSON or not window then
	return {-5}
end

-- 3) 校验活动时间。时间窗由 Go 在写缓存时拼成 "开始:结束"，
--    这里用纯字符串切分，不用解析 JSON，省掉一次解析开销。
local sep = string.find(window, ":", 1, true)
if not sep then
	return {-5}
end
local startAt = tonumber(string.sub(window, 1, sep - 1))
local endAt = tonumber(string.sub(window, sep + 1))
local now = tonumber(ARGV[3])
if now < startAt then
	return {-6}
end
if now > endAt then
	return {-7}
end

local item = cjson.decode(itemJSON)
local perUserLimit = tonumber(item.per_user_limit)
local totalStock = tonumber(item.total_stock)
local quantity = tonumber(ARGV[1])

-- 4) 幂等：同一个 request_id 只允许成功一次。
if redis.call("EXISTS", KEYS[3]) == 1 then
	return {-3}
end

-- 5) 库存。key 不存在时用总库存初始化，让第一次抢购自动把库存建起来。
local stockValue = redis.call("GET", KEYS[1])
if not stockValue then
	stockValue = totalStock
	redis.call("SET", KEYS[1], stockValue)
end

local stock = tonumber(stockValue)
if stock < quantity then
	return {-1}
end

-- 6) 每人限购。用「已买 + 本次 > 上限」一次判掉两种情况：
--    本次买太多，以及之前买过导致累计超限。
local bought = tonumber(redis.call("GET", KEYS[2]) or "0")
if bought + quantity > perUserLimit then
	return {-2}
end

-- 7) 真正扣减：库存减少、用户已买增加、写下幂等键。
redis.call("DECRBY", KEYS[1], quantity)
redis.call("INCRBY", KEYS[2], quantity)
redis.call("SET", KEYS[3], "1", "EX", tonumber(ARGV[2]))

-- 商品 JSON 顺手带回给 Go，省掉一次 GET。
return {1, itemJSON}
`

// errCacheNotReady 表示 Redis 里还没有这个秒杀商品的缓存。
// 它不是失败，只是一个信号：调用方要回源 MySQL 把缓存补齐，然后再试一次。
var errCacheNotReady = errors.New("秒杀商品缓存还没准备好")

// Cache 封装秒杀模块使用的 Redis 操作。
type Cache struct {
	client *redisclient.Client
}

// NewCache 创建秒杀 Redis 缓存。
func NewCache(client *redisclient.Client) *Cache {
	return &Cache{client: client}
}

// FastPathRequest 是一次抢购在热路径上需要的全部输入。
// 单独打包成结构体，是因为参数太多，平铺在函数签名里容易传错顺序。
type FastPathRequest struct {
	RequestID  string        // 幂等键
	ItemID     uint          // 秒杀商品 ID
	UserID     uint          // 抢购用户 ID
	Quantity   int           // 本次购买数量
	Now        time.Time     // 当前时间，供脚本校验活动时间
	RateLimit  int           // 限流窗口内最多放行多少次
	RateWindow time.Duration // 限流窗口长度
}

// TrySeckill 用一次 Redis 往返完成「限流 + 读缓存 + 校验活动时间 + 防重 + 限购 + 预扣库存」。
//
// 成功时返回商品信息（脚本顺手带回来的，不需要再查一次 Redis 或 MySQL）。
// 返回 errCacheNotReady 表示 Redis 里还没有这个商品的缓存，调用方应回源 MySQL 填充后重试。
func (c *Cache) TrySeckill(ctx context.Context, req FastPathRequest) (*Item, error) {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", req.ItemID)
	userKey := fmt.Sprintf("seckill:item:%d:user:%d", req.ItemID, req.UserID)
	requestKey := fmt.Sprintf("seckill:request:%s", req.RequestID)
	itemKey := fmt.Sprintf("seckill:item:%d:info", req.ItemID)
	windowKey := fmt.Sprintf("seckill:item:%d:window", req.ItemID)

	// 限流 key 不再带窗口编号：滑动窗口是「一条 key 记录最近一个窗口内所有被放行的请求」，
	// key 靠 PEXPIRE 在最后一次放行之后自然过期，不需要按窗口翻页。
	rateWindow := req.RateWindow
	if rateWindow <= 0 {
		rateWindow = time.Second
	}
	rateWindowMs := rateWindow.Milliseconds()
	if rateWindowMs < 1 {
		rateWindowMs = 1000
	}
	rateKey := fmt.Sprintf("ratelimit:seckill:user:%d", req.UserID)

	raw, err := c.client.Eval(
		ctx,
		hotPathLuaScript,
		[]string{stockKey, userKey, requestKey, itemKey, windowKey, rateKey},
		req.Quantity,        // ARGV[1]
		requestIDTTLSeconds, // ARGV[2]
		req.Now.Unix(),      // ARGV[3]
		req.RateLimit,       // ARGV[4]
		rateWindowMs,        // ARGV[5]
		req.Now.UnixMilli(), // ARGV[6]
		req.RequestID,       // ARGV[7]
	).Result()
	if err != nil {
		// 脚本返回 nil 或 false 时，go-redis 会返回 redis.Nil。
		if errors.Is(err, redis.Nil) {
			return nil, ErrDedectUnknown
		}
		return nil, err
	}

	// 脚本返回的是 Lua 表，go-redis 会把它转成 []interface{}：
	// 整数元素是 int64，字符串元素是 string。
	values, ok := raw.([]interface{})
	if !ok || len(values) == 0 {
		return nil, ErrDedectUnknown
	}

	code, ok := values[0].(int64)
	if !ok {
		return nil, ErrDedectUnknown
	}

	switch code {
	case 1:
		// 成功，下面继续解出商品信息。
	case -1:
		return nil, ErrInsufficientStock
	case -2:
		return nil, ErrPerUserLimitExceeded
	case -3:
		return nil, ErrDuplicateRequest
	case -4:
		return nil, ErrRateLimited
	case -5:
		return nil, errCacheNotReady
	case -6:
		return nil, ErrActivityNotStarted
	case -7:
		return nil, ErrActivityEnded
	default:
		return nil, ErrDedectUnknown
	}

	if len(values) < 2 {
		return nil, ErrDedectUnknown
	}
	itemJSON, ok := values[1].(string)
	if !ok {
		return nil, ErrDedectUnknown
	}

	var item Item
	if err := json.Unmarshal([]byte(itemJSON), &item); err != nil {
		return nil, err
	}

	return &item, nil
}

// WarmUp 把秒杀商品信息和它所属活动的时间窗写进 Redis。
// 什么时候用：热路径返回 errCacheNotReady（缓存过期、或者服务刚重启）时，
// 调用方回源 MySQL 拿到数据，用这个方法补齐缓存，然后再试一次。
//
// 为什么要单独存一个「时间窗」key：热路径在 Lua 里校验活动时间，
// 而 Lua 解析 RFC3339 格式的时间很不方便。这里提前把时间换算成两个 Unix 秒数拼成
// "开始:结束"，脚本只要切一刀就能拿到，既快又不依赖时间格式。
//
// 为什么时间窗挂在商品 ID 上而不是活动 ID 上：抢购接口只拿得到商品 ID，
// 如果要按活动 ID 存，就得先在 Lua 里解析商品 JSON 才知道活动 ID，白绕一层；
// 而一个商品只属于一场活动，用商品 ID 做 key 一样唯一。
func (c *Cache) WarmUp(ctx context.Context, item *Item, activity *Activity) error {
	itemKey := fmt.Sprintf("seckill:item:%d:info", item.ID)
	windowKey := fmt.Sprintf("seckill:item:%d:window", item.ID)

	data, err := json.Marshal(item)
	if err != nil {
		return err
	}

	window := fmt.Sprintf("%d:%d", activity.StartAt.Unix(), activity.EndAt.Unix())

	// 用一次 Pipeline 把两条写命令一起发出去，冷路径也少一次往返。
	pipe := c.client.Pipeline()
	pipe.Set(ctx, itemKey, data, seckillInfoTTL)
	pipe.Set(ctx, windowKey, window, seckillInfoTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// SetStock 直接设置某个秒杀商品在 Redis 中的剩余库存。
// 对账时用它把 Redis 修正成 MySQL 计算出的正确值。
func (c *Cache) SetStock(ctx context.Context, itemID uint, stock int) error {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)
	return c.client.Set(ctx, stockKey, stock, 0).Err()
}

// Stock 读取某个秒杀商品在 Redis 里的剩余库存。
//
// 第二个返回值表示「这个 key 到底存不存在」，必须跟 0 区分开：
// 如果不区分，「缓存还没建起来」和「库存已经扣到 0」都会返回 0，
// 演示页面上就会把「还没开始卖」显示成「已经售罄」。
//
// 它是给演示控制台和排查问题用的只读接口，不在抢购热路径上。
func (c *Cache) Stock(ctx context.Context, itemID uint) (int, bool, error) {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)

	value, err := c.client.Get(ctx, stockKey).Int()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}

	return value, true, nil
}

// RollbackDeduct 把一次已经预扣但最终没有落库的秒杀请求还回去。
// 什么时候用：消费端处理 Kafka 消息失败时，MySQL 事务会回滚，但 Redis 已经扣过了；
// 为了让两边重新对齐，需要把 Redis 库存加回来，并把用户已购买数量减回去。
func (c *Cache) RollbackDeduct(ctx context.Context, itemID, userID uint, quantity int) error {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)
	userKey := fmt.Sprintf("seckill:item:%d:user:%d", itemID, userID)

	result, err := c.client.Eval(ctx, rollbackLuaScript, []string{stockKey, userKey}, quantity).Int()
	if err != nil {
		return err
	}

	switch result {
	case -1:
		return errors.New("用户已购买数量不足")
	case 1:
		return nil
	default:
		return errors.New("Redis出现未知错误")
	}
}
