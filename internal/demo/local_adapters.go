package demo

import (
	"context"
	"time"

	"shopping_market/internal/inventory"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

// 下面四个适配器给单体模式使用：接口方法带 ctx，本地 Service 大多不带。
// 适配器负责把这个差异抹平，让 demo.Handler 在两种模式下长得完全一样。

type localUserRegistrar struct {
	service *user.Service
}

var _ UserRegistrar = (*localUserRegistrar)(nil)

func NewLocalUserRegistrar(service *user.Service) UserRegistrar {
	return &localUserRegistrar{service: service}
}

func (a *localUserRegistrar) Register(ctx context.Context, username, password string) (*user.User, error) {
	return a.service.Register(username, password)
}

type localProductStore struct {
	service *product.Service
}

var _ ProductStore = (*localProductStore)(nil)

func NewLocalProductStore(service *product.Service) ProductStore {
	return &localProductStore{service: service}
}

func (a *localProductStore) List(ctx context.Context) ([]product.Product, error) {
	return a.service.List()
}

func (a *localProductStore) CreateProduct(ctx context.Context, name, description string, price float64) (*product.Product, error) {
	return a.service.Create(name, description, price)
}

func (a *localProductStore) CreateSKU(ctx context.Context, productID uint, spec string, price float64) (*product.SKU, error) {
	return a.service.CreateSKU(productID, spec, price)
}

func (a *localProductStore) FindSKUByProductAndSpec(ctx context.Context, productID uint, spec string) (*product.SKU, error) {
	return a.service.FindSKUByProductAndSpec(productID, spec)
}

type localStockStore struct {
	service *inventory.Service
}

var _ StockStore = (*localStockStore)(nil)

func NewLocalStockStore(service *inventory.Service) StockStore {
	return &localStockStore{service: service}
}

func (a *localStockStore) Get(ctx context.Context, skuID uint) (*inventory.Stock, error) {
	return a.service.Get(skuID)
}

func (a *localStockStore) CreateStock(ctx context.Context, skuID uint, quantity int) (*inventory.Stock, error) {
	return a.service.Create(skuID, quantity)
}

type localSeckillAdmin struct {
	service *seckill.Service
}

var _ SeckillAdmin = (*localSeckillAdmin)(nil)

func NewLocalSeckillAdmin(service *seckill.Service) SeckillAdmin {
	return &localSeckillAdmin{service: service}
}

func (a *localSeckillAdmin) CreateActivity(ctx context.Context, name string, startAt, endAt time.Time) (*seckill.Activity, error) {
	return a.service.CreateActivity(name, startAt, endAt)
}

func (a *localSeckillAdmin) ListActivities(ctx context.Context) ([]seckill.Activity, error) {
	return a.service.ListActivities()
}

func (a *localSeckillAdmin) CreateItem(ctx context.Context, activityID, skuID uint, price float64, totalStock, perUserLimit int) (*seckill.Item, error) {
	return a.service.CreateItem(activityID, skuID, price, totalStock, perUserLimit)
}

func (a *localSeckillAdmin) ListItems(ctx context.Context, activityID uint) ([]seckill.Item, error) {
	return a.service.ListItems(activityID)
}

func (a *localSeckillAdmin) RemainingStock(ctx context.Context, itemID uint) (int, bool, error) {
	return a.service.RemainingStock(ctx, itemID)
}

func (a *localSeckillAdmin) WarmUpStock(ctx context.Context, itemID uint) error {
	return a.service.WarmUpStock(ctx, itemID)
}

func (a *localSeckillAdmin) Pending(ctx context.Context) (int64, error) {
	return a.service.Pending(), nil
}
