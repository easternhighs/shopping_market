package ratelimit

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	redispkg "shopping_market/internal/pkg/redis"
)

// slidingWindowLuaScript 用「滑动窗口」判断一次请求能不能放行。
//
// 为什么不用固定窗口（老实现是 INCR 一个带窗口编号的 key）：
// 固定窗口在窗口边界上会放过接近两倍的流量——比如限制 100 次/秒，
// 在第 1 秒的最后 10 毫秒打 100 次、第 2 秒的头 10 毫秒再打 100 次，
// 两次在各自的窗口里都「没超」，可现实里这 20 毫秒内已经过了 200 次。
// 窗口编号只会翻页，不会记住「刚刚那批也才过去几毫秒」。
//
// 这个脚本把每次「放行」的时刻记进一个 ZSET，判断依据只有一条：
// 最近 windowMs 毫秒内放行过几次。没有窗口翻页，也就没有那个边界瞬间。
//
//	KEYS[1] = 限流用的 ZSET
//	ARGV[1] = nowMs     当前 Unix 毫秒
//	ARGV[2] = windowMs  窗口长度（毫秒）
//	ARGV[3] = limit     窗口内最多放行多少次
//	ARGV[4] = member    本次请求的唯一标识（同一毫秒内的并发请求不能互相覆盖）
//
// 返回 1 = 放行，0 = 拒绝。
const slidingWindowLuaScript = `
local nowMs = tonumber(ARGV[1])
local windowMs = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
-- 先把滑出窗口的记录删掉，再数剩下的：这就是「最近一个窗口内放行过几次」。
redis.call("ZREMRANGEBYSCORE", KEYS[1], 0, nowMs - windowMs)
if redis.call("ZCARD", KEYS[1]) >= limit then
	return 0
end
-- 只有放行的请求才记进窗口，否则窗口会被自己的拒绝记录越填越满。
redis.call("ZADD", KEYS[1], nowMs, ARGV[4])
redis.call("PEXPIRE", KEYS[1], windowMs)
return 1
`

// slidingSeq 给「同一个进程、同一毫秒」内的请求提供互不相同的成员后缀。
// 只用毫秒时间戳当成员的话，同一毫秒里的并发请求会互相覆盖，计数偏少、限流被放水。
var slidingSeq atomic.Uint64

// RedisLimiter 是基于 Redis 的滑动窗口限流器。
//
// 和秒杀热路径里的限流是同一套思路，但没有复用同一段脚本：
// 秒杀那份是嵌在「一次往返做完全部校验」的大脚本里的，为的是省掉一次网络往返，
// 这里则是一个独立的、给管理类接口按 IP 用的通用限流器。两者的取舍不同。
type RedisLimiter struct {
	client *redispkg.Client
}

// NewRedisLimiter 创建 Redis 限流器。
func NewRedisLimiter(client *redispkg.Client) *RedisLimiter {
	return &RedisLimiter{client: client}
}

// Allow 判断当前 key 是否还能通过滑动窗口限流。
func (l *RedisLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if window <= 0 {
		window = time.Second
	}
	windowMs := window.Milliseconds()
	if windowMs < 1 {
		windowMs = 1
	}

	nowMs := time.Now().UnixMilli()
	member := fmt.Sprintf("%d-%d", nowMs, slidingSeq.Add(1))

	// key 里不再带窗口编号：窗口是滑动的，没有翻页这一说；
	// 整条 key 靠 PEXPIRE 在最后一次放行之后过期，不需要额外清理。
	redisKey := "ratelimit:" + key

	allowed, err := l.client.Eval(
		ctx,
		slidingWindowLuaScript,
		[]string{redisKey},
		nowMs,    // ARGV[1]
		windowMs, // ARGV[2]
		limit,    // ARGV[3]
		member,   // ARGV[4]
	).Int()
	if err != nil {
		return false, err
	}
	return allowed == 1, nil
}
