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
