package ratelimit

import (
	"context"
	"time"
)

// Limiter 是限流器的统一接口。
// 你可以把它理解成游乐场入口的“人数闸机”：每来一个请求，就问一次“现在还能不能进”。
// 允许就返回 true，拒绝就返回 false。
type Limiter interface {
	// Allow 判断 key 在 window 时间窗口内，请求次数是否小于 limit。
	// key 通常可以是“用户ID”或“客户端IP”，用于区分不同请求来源。
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}
