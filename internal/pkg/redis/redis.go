package redis

import (
	"context"

	goredis "github.com/redis/go-redis/v9"
)

// Client 是对 go-redis 客户端的轻量封装。
// 后续秒杀模块通过它执行 Redis Lua 脚本。
type Client struct {
	*goredis.Client
}

// New 创建 Redis 客户端。
func New(addr string) *Client {
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	return &Client{Client: client}
}

// Ping 检查 Redis 是否连通。
func (c *Client) Ping(ctx context.Context) error {
	return c.Client.Ping(ctx).Err()
}
