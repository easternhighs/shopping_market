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

// seckillInfoTTL 是秒杀商品/活动信息缓存的有效期。
// 信息类数据（价格、限购、活动时间）改动不频繁，缓存一小段时间即可；
// 库存这种频繁变化的数据不用它缓存，走 :stock key。
const seckillInfoTTL = 5 * time.Minute

// KEYS[1]:当前秒杀商品的Redis库存
// KEYS[2]:当前秒杀商品的Redis用户已购买数量
// ARGV[1]:当前秒杀商品的购买数量
// ARGV[2]:当前秒杀商品的每人限购数量
// ARGV[3]:当前秒杀商品的总库存数量,只用于在Redis中不存在库存时初始化库存
const proDeductLuaScript = `
local stockValue = redis.call("GET", KEYS[1])
if not stockValue then
	stockValue = ARGV[3] -- 如果库存不存在，则使用传入的总库存值
	redis.call("SET", KEYS[1], stockValue)
end

-- tonumber将字符串转换为数字
local stock = tonumber(stockValue)	-- 获取当前库存
local quantity = tonumber(ARGV[1])	-- 获取购买数量
local perUserLimit = tonumber(ARGV[2])	-- 获取每人限购数量

if stock < quantity then
	return -1 -- 库存不足
end

local bought = tonumber(redis.call("GET", KEYS[2]) or "0") -- 获取用户已购买数量
if bought + quantity > perUserLimit then
	return -2 -- 超过每人限购数量
end

-- 扣减库存
redis.call("DECRBY", KEYS[1], quantity)
-- 记录用户已购买数量
redis.call("INCRBY", KEYS[2], quantity)

return 1 -- 成功
`

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

// checkAndPreDeductLuaScript 把“requestID 查重、用户限购、库存预扣”合并成一次 Redis 原子操作。
//
// 参数约定：
//
//	KEYS[1] = 商品库存 key
//	KEYS[2] = 该用户在该商品的已购买数量 key
//	KEYS[3] = requestID 幂等 key
//	ARGV[1] = quantity
//	ARGV[2] = perUserLimit
//	ARGV[3] = totalStock，仅当库存 key 不存在时用于初始化
//	ARGV[4] = requestID key 的过期时间（秒）
//
// 返回值约定：
//
//	1   = 成功
//	-1  = 库存不足
//	-2  = 超过每人限购数量
//	-3  = requestID 已存在
const checkAndPreDeductLuaScript = `
local stockValue = redis.call("GET", KEYS[1])
if not stockValue then
	stockValue = ARGV[3]
	redis.call("SET", KEYS[1], stockValue)
end

local stock = tonumber(stockValue)
local quantity = tonumber(ARGV[1])
local perUserLimit = tonumber(ARGV[2])

if redis.call("EXISTS", KEYS[3]) == 1 then
	return -3
end

if stock < quantity then
	return -1
end

local bought = tonumber(redis.call("GET", KEYS[2]) or "0")
if bought + quantity > perUserLimit then
	return -2
end

redis.call("DECRBY", KEYS[1], quantity)
redis.call("INCRBY", KEYS[2], quantity)

local ttl = tonumber(ARGV[4])
redis.call("SET", KEYS[3], "1", "EX", ttl)

return 1
`

// Cache 封装秒杀模块使用的 Redis 操作。
type Cache struct {
	client *redisclient.Client
}

// NewCache 创建秒杀 Redis 缓存。
func NewCache(client *redisclient.Client) *Cache {
	return &Cache{client: client}
}

// PreDeduct 用 Redis Lua 脚本原子预扣库存。
func (c *Cache) PreDeduct(ctx context.Context, itemID, userID uint, quantity, totalStock, perUserLimit int) error {
	//通过Lua脚本开启事务，保证原子性，将“库存检查-检查限购-扣库存-记录用户购买数量”绑定成原子操作
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)
	userKey := fmt.Sprintf("seckill:item:%d:user:%d", itemID, userID)

	//固定语法，Eval(ctx, script, keys, args...) 的语义固定为：
	//  - keys 切片 → 脚本里的 KEYS[1]、KEYS[2] …（按顺序 1-based）
	//  - 可变参数 args... → 脚本里的 ARGV[1]、ARGV[2] …（按顺序 1-based）
	//  - 返回 *redis.Cmd，.Int() 把 Redis 的 整数回复 转成 Go 的 int。
	result, err := c.client.Eval(
		ctx,                         // 上下文：超时/取消控制
		proDeductLuaScript,          // Lua 脚本源码（字符串）
		[]string{stockKey, userKey}, // KEYS[1], KEYS[2]
		quantity,                    // ARGV[1]
		perUserLimit,                // ARGV[2]
		totalStock,                  // ARGV[3]
	).Int()
	if err != nil {
		//脚本返回nil或者false时，go-redis会返回redis.Nil
		if errors.Is(err, redis.Nil) {
			return ErrDedectUnknown
		}
		return err
	}

	switch result {
	case 1:
		return nil
	case -1:
		return ErrInsufficientStock
	case -2:
		return errors.New("超出每人购买限制")
	default:
		return errors.New("redis lua 出现未知错误")
	}
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

// CheckAndPreDeduct 在 Redis 中一次完成“requestID 防重、用户限购、库存预扣”。
// 它把三次判断合并成一次 Lua 调用，避免高并发下 Redis 往返次数过多，
// 也避免“先查后扣”被其他请求插入导致的超卖。
func (c *Cache) CheckAndPreDeduct(ctx context.Context, requestID string, itemID, userID uint, quantity, totalStock, perUserLimit int) error {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)
	userKey := fmt.Sprintf("seckill:item:%d:user:%d", itemID, userID)
	requestKey := fmt.Sprintf("seckill:request:%s", requestID)

	result, err := c.client.Eval(
		ctx,
		checkAndPreDeductLuaScript,
		[]string{stockKey, userKey, requestKey},
		quantity,
		perUserLimit,
		totalStock,
		86400,
	).Int()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrDedectUnknown
		}
		return err
	}

	switch result {
	case 1:
		return nil
	case -1:
		return ErrInsufficientStock
	case -2:
		return errors.New("超出每人购买限制")
	case -3:
		return errors.New("重复的 request_id")
	default:
		return errors.New("redis lua 出现未知错误")
	}

}

// GetItem 从 Redis 读取秒杀商品缓存。
// 未命中时返回 redis.Nil，调用方需要回源 MySQL 并调用 SetItem 写回缓存。
func (c *Cache) GetItem(ctx context.Context, itemID uint) (*Item, error) {
	// 注意：这里用 :info 后缀，和 :stock 库存 key 区分开，两者不能共用。
	itemKey := fmt.Sprintf("seckill:item:%d:info", itemID)
	data, err := c.client.Get(ctx, itemKey).Bytes()
	if err != nil {
		// 未命中时返回 redis.Nil，调用方需要回源 MySQL 再写回缓存。
		return nil, err
	}

	var item Item
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, err
	}

	return &item, nil
}

// SetItem 把秒杀商品写入 Redis 缓存。
func (c *Cache) SetItem(ctx context.Context, item *Item) error {
	itemKey := fmt.Sprintf("seckill:item:%d:info", item.ID)
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}

	return c.client.Set(ctx, itemKey, data, seckillInfoTTL).Err()
}

// GetActivity 从 Redis 读取秒杀活动缓存。
// 未命中时返回 redis.Nil，调用方需要回源 MySQL 并调用 SetActivity 写回缓存。
func (c *Cache) GetActivity(ctx context.Context, activityID uint) (*Activity, error) {
	key := fmt.Sprintf("seckill:activity:%d:info", activityID)
	data, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}

	var activity Activity
	if err = json.Unmarshal(data, &activity); err != nil {
		return nil, err
	}

	return &activity, nil
}

// SetActivity 把秒杀活动写入 Redis 缓存。
func (c *Cache) SetActivity(ctx context.Context, activity *Activity) error {
	key := fmt.Sprintf("seckill:activity:%d:info", activity.ID)

	byteActivity, err := json.Marshal(activity)
	if err != nil {
		return err
	}

	return c.client.Set(ctx, key, byteActivity, seckillInfoTTL).Err()
}
