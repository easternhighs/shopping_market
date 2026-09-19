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
//	KEYS[6] = 限流计数 key（固定窗口）
//	ARGV[1] = quantity       本次购买数量
//	ARGV[2] = requestTTL     幂等 key 的过期秒数
//	ARGV[3] = now            当前 Unix 秒（由 Go 传入，避免脚本里的时间不确定）
//	ARGV[4] = rateLimitMax   当前窗口内最多放行多少次
//	ARGV[5] = rateWindowSec  限流窗口的秒数，用作限流 key 的过期时间
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
-- 1) 限流。固定窗口：同一个窗口内每来一次就 +1，超过上限直接拒绝。
--    放在最前面是有意的：被限流的请求连后面的库存都不碰，代价最小。
local count = redis.call("INCR", KEYS[6])
if count == 1 then
	redis.call("EXPIRE", KEYS[6], tonumber(ARGV[5]))
end
if count > tonumber(ARGV[4]) then
	return {-4}
end

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

	// 限流 key 里带上窗口编号：窗口一翻页，key 就自然换成了新的，
	// 旧 key 靠 EXPIRE 自己过期，不需要额外清理。
	rateWindow := req.RateWindow
	if rateWindow <= 0 {
		rateWindow = time.Second
	}
	windowSec := int64(rateWindow / time.Second)
	if windowSec < 1 {
		windowSec = 1
	}
	windowID := req.Now.UnixMilli() / rateWindow.Milliseconds()
	rateKey := fmt.Sprintf("ratelimit:seckill:user:%d:%d", req.UserID, windowID)

	raw, err := c.client.Eval(
		ctx,
		hotPathLuaScript,
		[]string{stockKey, userKey, requestKey, itemKey, windowKey, rateKey},
		req.Quantity,        // ARGV[1]
		requestIDTTLSeconds, // ARGV[2]
		req.Now.Unix(),      // ARGV[3]
		req.RateLimit,       // ARGV[4]
		windowSec,           // ARGV[5]
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
