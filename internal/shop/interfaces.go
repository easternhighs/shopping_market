package shop

import (
	"context"
	"time"

	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
)

// ProductReader 是「前台眼里的商品服务」。
//
// 为什么要定义接口，而不是像以前那样直接写 *product.Service：
//   - 单体模式下，背后是同一个进程里的函数调用（本地适配器）；
//   - 独立模式下，背后是另一个进程的 HTTP 接口（HTTP 客户端）。
//
// 前台页面只关心「拿到商品列表」和「拿到规格列表」，不关心数据是从函数来的
// 还是从网络来的。把这层差异挡在接口后面，页面代码一行都不用改。
type ProductReader interface {
	List(ctx context.Context) ([]product.Product, error)
	ListSKUsByProductIDs(ctx context.Context, ids []uint) ([]product.SKU, error)
}

// SeckillReader 是「前台眼里的秒杀服务」。
//
// 这里的方法都是只读的：前台只看活动、商品、剩余库存和我的订单，
// 真正抢购走的是秒杀服务自己的 /seckill/items/:id/orders，不经过 shop 包。
type SeckillReader interface {
	ListActivities(ctx context.Context) ([]seckill.Activity, error)
	ListItems(ctx context.Context, activityID uint) ([]seckill.Item, error)
	RemainingStock(ctx context.Context, itemID uint) (int, bool, error)
	PayTimeout(ctx context.Context) (time.Duration, error)
	ListUserOrders(ctx context.Context, userID uint) ([]seckill.Order, error)
}
