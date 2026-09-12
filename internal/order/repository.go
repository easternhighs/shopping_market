package order

import "gorm.io/gorm"

// Repository 定义订单数据的读写接口。
type Repository interface {
	FindByRequestID(requestID string) (*Order, error)
	FindByID(id uint) (*Order, error)
	FindItemsByOrderID(orderID uint) ([]Item, error)
	Transaction(fn func(tx *gorm.DB) error) error
	Create(tx *gorm.DB, o *Order) error
	CreateItems(tx *gorm.DB, items []Item) error
	UpdateStatus(orderID uint, status string) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建订单仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindByRequestID 按幂等键查询订单。
func (r *repository) FindByRequestID(requestID string) (*Order, error) {
	var o Order
	if err := r.db.Where("request_id = ?", requestID).First(&o).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// FindByID 按 ID 查询订单。
func (r *repository) FindByID(id uint) (*Order, error) {
	var o Order
	if err := r.db.First(&o, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// FindItemsByOrderID 查询订单明细。
func (r *repository) FindItemsByOrderID(orderID uint) ([]Item, error) {
	var items []Item
	if err := r.db.Where("order_id = ?", orderID).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Transaction 开启一个数据库事务。
func (r *repository) Transaction(fn func(tx *gorm.DB) error) error {
	return r.db.Transaction(fn)
}

// Create 在事务里创建订单。
func (r *repository) Create(tx *gorm.DB, o *Order) error {
	return tx.Create(o).Error
}

// CreateItems 在事务里批量创建订单明细。
func (r *repository) CreateItems(tx *gorm.DB, items []Item) error {
	return tx.Create(&items).Error
}

// UpdateStatus 更新订单状态。
func (r *repository) UpdateStatus(orderID uint, status string) error {
	return r.db.Model(&Order{}).Where("id = ?", orderID).Update("status", status).Error
}
