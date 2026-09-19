package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client 是对 go-redis 客户端的轻量封装。
// 后续秒杀模块通过它执行 Redis Lua 脚本。
type Client struct {
	*goredis.Client
}

// New 创建 Redis 客户端。
//
// 这里把连接池参数显式写出来，而不是用默认值：
//   - PoolSize：池子里最多同时有多少条连接。每个正在执行中的 Redis 命令都要独占一条，
//     所以并发一高，池子太小会让请求排队等连接；
//   - MinIdleConns：常备的空闲连接数，避免流量突然涨上来时还要现连（建连接比发命令慢得多）；
//   - PoolTimeout：池子被占满时最多等多久要一条连接。等不到就报错返回，
//     总比让请求无限期挂着、把 goroutine 和内存都堆起来要好。
func New(addr string) *Client {
	client := goredis.NewClient(&goredis.Options{
		Addr:         addr,
		PoolSize:     512,
		MinIdleConns: 16,
		PoolTimeout:  2 * time.Second,
	})
	return &Client{Client: client}
}

// Ping 检查 Redis 是否连通。
func (c *Client) Ping(ctx context.Context) error {
	return c.Client.Ping(ctx).Err()
}
