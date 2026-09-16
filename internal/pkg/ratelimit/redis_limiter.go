package ratelimit

import (
	"context"
	"fmt"
	"time"

	redispkg "shopping_market/internal/pkg/redis"
)

// RedisLimiter 是基于 Redis 的固定窗口限流器。
// 固定窗口很好理解：比如“1 分钟内最多 10 次”，窗口从 0 秒开始，到 60 秒结束，周而复始。
type RedisLimiter struct {
	client *redispkg.Client
}

// NewRedisLimiter 创建 Redis 限流器。
func NewRedisLimiter(client *redispkg.Client) *RedisLimiter {
	return &RedisLimiter{client: client}
}

// Allow 判断当前 key 是否还能通过固定窗口限流。
func (l *RedisLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	windowID := time.Now().UnixMilli() / window.Milliseconds()

	// 拼接RedisKey
	redisKey := fmt.Sprintf("ratelimit:%s:%d", key, windowID)
	count, err := l.client.Incr(ctx, redisKey).Result()
	if err != nil {
		return false, err
	}

	// 第一次创建key时设置过期时间
	if count == 1 {
		if err := l.client.Expire(ctx, redisKey, window+time.Millisecond*2).Err(); err != nil {
			return false, err
		}
	}

	return count <= int64(limit), nil

}
