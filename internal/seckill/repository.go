package seckill

import (
	"errors"

	"gorm.io/gorm"
)

// Repository 定义秒杀模块的 MySQL 读写接口。
type Repository interface {
	FindActivityByID(id uint) (*Activity, error)
	ListActivities() ([]Activity, error)
	CreateActivity(a *Activity) error
	FindItemByID(id uint) (*Item, error)
	ListItems() ([]Item, error)
	ListItemsByActivityID(activityID uint) ([]Item, error)
	CreateItem(i *Item) error
	FindOrderByRequestID(requestID string) (*Order, error)
	FindOrderByUserAndItem(userID, itemID uint) (*Order, error)
	CreateOrder(o *Order) error
	FindOrderByRequestIDWithTx(tx *gorm.DB, requestID string) (*Order, error)
	FindOrderByUserAndItemWithTx(tx *gorm.DB, userID, itemID uint) (*Order, error)
	CreateOrderWithTx(tx *gorm.DB, o *Order) error
	Transaction(fn func(tx *gorm.DB) error) error
	DeductStock(tx *gorm.DB, itemID uint, quantity int) error

	// 下面这组是“批量落库”新增的方法：一次处理一批消息，而不是一条一条来。
	// 单条版的方法暂时保留，改造完成后可以删掉。
	FindOrdersByRequestIDsWithTx(tx *gorm.DB, requestIDs []string) ([]Order, error)
	FindOrdersByUserAndItemWithTx(tx *gorm.DB, itemID uint, userIDs []uint) ([]Order, error)
	CreateOrdersWithTx(tx *gorm.DB, orders []Order) error
	DeductStockBatch(tx *gorm.DB, itemID uint, totalQuantity int) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建秒杀仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindActivityByID 按 ID 查询秒杀活动。
func (r *repository) FindActivityByID(id uint) (*Activity, error) {
	var a Activity
	if err := r.db.First(&a, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// ListActivities 查询全部秒杀活动。
func (r *repository) ListActivities() ([]Activity, error) {
	var activities []Activity
	if err := r.db.Find(&activities).Error; err != nil {
		return nil, err
	}
	return activities, nil
}

// CreateActivity 创建秒杀活动。
func (r *repository) CreateActivity(a *Activity) error {
	return r.db.Create(a).Error
}

// FindItemByID 按 ID 查询秒杀商品。
func (r *repository) FindItemByID(id uint) (*Item, error) {
	var item Item
	if err := r.db.First(&item, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

// ListItems 查询全部秒杀商品，供对账任务遍历使用。
func (r *repository) ListItems() ([]Item, error) {
	var items []Item
	if err := r.db.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// ListItemsByActivityID 查询某场活动下的全部秒杀商品。
func (r *repository) ListItemsByActivityID(activityID uint) ([]Item, error) {
	var items []Item
	if err := r.db.Where("activity_id = ?", activityID).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// CreateItem 创建秒杀商品。
func (r *repository) CreateItem(i *Item) error {
	return r.db.Create(i).Error
}

// FindOrderByRequestID 按幂等键查询秒杀订单。
func (r *repository) FindOrderByRequestID(requestID string) (*Order, error) {
	var o Order
	if err := r.db.Where("request_id = ?", requestID).First(&o).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// FindOrderByUserAndItem 查询某个用户是否已经抢购过某个秒杀商品。
func (r *repository) FindOrderByUserAndItem(userID, itemID uint) (*Order, error) {
	var o Order
	err := r.db.Where("user_id = ? AND item_id = ?", userID, itemID).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateOrder 创建秒杀订单。
func (r *repository) CreateOrder(o *Order) error {
	return r.db.Create(o).Error
}

// FindOrderByRequestIDWithTx 在事务里按幂等键查询秒杀订单。
// 和 FindOrderByRequestID 的区别是：它使用事务连接，保证“查重”和“创建”处在同一个事务里。
func (r *repository) FindOrderByRequestIDWithTx(tx *gorm.DB, requestID string) (*Order, error) {
	var o Order
	err := tx.Where("request_id = ?", requestID).First(&o).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &o, nil
}

// FindOrderByUserAndItemWithTx 在事务里查询某个用户是否已经抢购过某个秒杀商品。
func (r *repository) FindOrderByUserAndItemWithTx(tx *gorm.DB, userID, itemID uint) (*Order, error) {
	var o Order
	err := tx.Where("user_id = ? AND item_id = ?", userID, itemID).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateOrderWithTx 在事务里创建秒杀订单。
// 和 CreateOrder 的区别是：它使用事务连接，这样订单可以和库存扣减一起成功或一起失败。
func (r *repository) CreateOrderWithTx(tx *gorm.DB, o *Order) error {
	return tx.Create(o).Error
}

// Transaction 开启一个数据库事务。
func (r *repository) Transaction(fn func(tx *gorm.DB) error) error {
	return r.db.Transaction(fn)
}

// DeductStock 在事务里最终扣减秒杀库存。
func (r *repository) DeductStock(tx *gorm.DB, itemID uint, quantity int) error {
	result := tx.Model(&Item{}).Where("id = ? AND sold + ? <= total_stock", itemID, quantity).Update("sold", gorm.Expr("sold + ?", quantity))

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return ErrInsufficientStock
	}
	return nil
}

// FindOrdersByRequestIDsWithTx 一次查出这一批里“已经存在”的 request_id。
//
// 原来每条消息都要单独查一次重（N 次查询）；现在用一条 IN 查询把整批问完，
// 网络往返从 N 次降到 1 次。查出来的记录只用来标记“哪些是重复消息”。
func (r *repository) FindOrdersByRequestIDsWithTx(tx *gorm.DB, requestIDs []string) ([]Order, error) {
	if len(requestIDs) == 0 {
		return nil, nil
	}

	var orders []Order
	if err := tx.Where("request_id IN ?", requestIDs).Find(&orders).Error; err != nil {
		return nil, err
	}
	return orders, nil
}

// FindOrdersByUserAndItemWithTx 一次查出“这批用户里已经买过这个商品”的记录，
// 用来在批量落库前筛掉会触发限购的消息。
func (r *repository) FindOrdersByUserAndItemWithTx(tx *gorm.DB, itemID uint, userIDs []uint) ([]Order, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}

	var orders []Order
	if err := tx.Where("item_id = ? AND user_id IN ?", itemID, userIDs).Find(&orders).Error; err != nil {
		return nil, err
	}
	return orders, nil
}

// CreateOrdersWithTx 在事务里批量插入订单。
//
// GORM 的 Create 传入切片时会自动拼成一条
// INSERT INTO seckill_orders (...) VALUES (...),(...),(...)，
// 所以 200 条订单只要一次网络往返、一次写盘，而不是 200 次。
func (r *repository) CreateOrdersWithTx(tx *gorm.DB, orders []Order) error {
	if len(orders) == 0 {
		return nil
	}
	return tx.Create(&orders).Error
}

// DeductStockBatch 在事务里一次性扣掉“某个秒杀商品”在这一批里卖出的全部数量。
//
// 这是本轮优化的核心：原来 N 条消息要 UPDATE 同一行 N 次，每次都要抢行锁；
// 现在整批只 UPDATE 一次，行锁也只抢 1 次。
//
// 和单条版 DeductStock 唯一的区别就是数量：这里加的是整批的 totalQuantity，
// 条件里的“卖完不超卖”也一起按总量判断。
func (r *repository) DeductStockBatch(tx *gorm.DB, itemID uint, totalQuantity int) error {
	if totalQuantity <= 0 {
		return nil
	}

	result := tx.Model(&Item{}).Where("id = ? AND sold + ? <= total_stock", itemID, totalQuantity).Update("sold", gorm.Expr("sold + ?", totalQuantity))

	// 先看 SQL 本身有没有报错（比如连接断了、表不存在）。
	// 少了这一步，出错的 RowsAffected 也是 0，就会被误判成“库存不足”，
	// 真正的原因被掩盖，排查问题时会很痛苦。
	if result.Error != nil {
		return result.Error
	}

	// RowsAffected 为 0 有两种可能：商品不存在，或者加上这一批就超过总库存了。
	// 对秒杀来说两种都按“库存不足”处理——宁可不卖，也绝不超卖。
	if result.RowsAffected == 0 {
		return ErrInsufficientStock
	}

	return nil
}
