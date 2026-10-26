package shop

import (
	"context"
	"time"

	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
)

// 下面两个适配器是「单体模式」用的：把它们包在现有 Service 外面，
// 让 shop.Handler 以为自己在和一个接口打交道，实际上还是进程内函数调用。
//
// 这样做的价值：拆分前后页面代码完全不动，回退也只需要换一个构造函数。

type localProductReader struct {
	service *product.Service
}

var _ ProductReader = (*localProductReader)(nil)

// NewLocalProductReader 把本地商品 Service 包成 ProductReader。
func NewLocalProductReader(service *product.Service) ProductReader {
	return &localProductReader{service: service}
}

func (a *localProductReader) List(ctx context.Context) ([]product.Product, error) {
	// 本地调用没有网络，不存在「请求被取消」这回事，ctx 在这里只是保持签名一致。
	return a.service.List()
}

func (a *localProductReader) ListSKUsByProductIDs(ctx context.Context, ids []uint) ([]product.SKU, error) {
	return a.service.ListSKUsByProductIDs(ids)
}

type localSeckillReader struct {
	service *seckill.Service
}

var _ SeckillReader = (*localSeckillReader)(nil)

// NewLocalSeckillReader 把本地秒杀 Service 包成 SeckillReader。
func NewLocalSeckillReader(service *seckill.Service) SeckillReader {
	return &localSeckillReader{service: service}
}

func (a *localSeckillReader) ListActivities(ctx context.Context) ([]seckill.Activity, error) {
	return a.service.ListActivities()
}

func (a *localSeckillReader) ListItems(ctx context.Context, activityID uint) ([]seckill.Item, error) {
	return a.service.ListItems(activityID)
}

func (a *localSeckillReader) RemainingStock(ctx context.Context, itemID uint) (int, bool, error) {
	return a.service.RemainingStock(ctx, itemID)
}

func (a *localSeckillReader) PayTimeout(ctx context.Context) (time.Duration, error) {
	// 注意：seckill.Service.PayTimeout 本身没有 context 参数。
	// 这里由适配器补上，接口形状不需要为了「本地实现没有 ctx」而妥协。
	return a.service.PayTimeout(), nil
}

func (a *localSeckillReader) ListUserOrders(ctx context.Context, userID uint) ([]seckill.Order, error) {
	return a.service.ListUserOrders(userID)
}
