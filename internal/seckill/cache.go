package seckill

import (
	"context"
	"errors"
	"fmt"

	redisclient "shopping_market/internal/pkg/redis"

	"github.com/redis/go-redis/v9"
)

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
