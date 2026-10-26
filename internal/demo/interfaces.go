package demo

import (
	"context"
	"time"

	"shopping_market/internal/inventory"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

// UserRegistrar 是控制台眼里的用户服务。
//
// 控制台造演示账号只需要「注册」这一件事，所以不把整个用户模块搬过来。
// 接口越小，独立模式要实现的 HTTP 客户端也越简单。
type UserRegistrar interface {
	Register(ctx context.Context, username, password string) (*user.User, error)
}

// ProductStore 是控制台眼里的商品服务。
type ProductStore interface {
	List(ctx context.Context) ([]product.Product, error)
	CreateProduct(ctx context.Context, name, description string, price float64) (*product.Product, error)
	CreateSKU(ctx context.Context, productID uint, spec string, price float64) (*product.SKU, error)
	FindSKUByProductAndSpec(ctx context.Context, productID uint, spec string) (*product.SKU, error)
}

// StockStore 是控制台眼里的库存服务。
type StockStore interface {
	Get(ctx context.Context, skuID uint) (*inventory.Stock, error)
	CreateStock(ctx context.Context, skuID uint, quantity int) (*inventory.Stock, error)
}

// SeckillAdmin 是控制台眼里的秒杀服务。
//
// 这些方法都只给演示面板 / 一键造数据使用，不参与抢购热路径。
type SeckillAdmin interface {
	CreateActivity(ctx context.Context, name string, startAt, endAt time.Time) (*seckill.Activity, error)
	ListActivities(ctx context.Context) ([]seckill.Activity, error)
	CreateItem(ctx context.Context, activityID, skuID uint, price float64, totalStock, perUserLimit int) (*seckill.Item, error)
	ListItems(ctx context.Context, activityID uint) ([]seckill.Item, error)
	RemainingStock(ctx context.Context, itemID uint) (int, bool, error)
	WarmUpStock(ctx context.Context, itemID uint) error
	Pending(ctx context.Context) (int64, error)
}
