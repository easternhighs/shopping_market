package payment

import (
	"time"

	"gorm.io/gorm"
)

// Repository 定义支付流水的读写接口。
type Repository interface {
	FindByOrderID(orderID uint) (*Payment, error)
	Create(p *Payment) error
	UpdateStatus(orderID uint, status string, paidAt *time.Time) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建支付仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindByOrderID 按订单 ID 查找支付流水。
func (r *repository) FindByOrderID(orderID uint) (*Payment, error) {
	var p Payment
	if err := r.db.Where("order_id = ?", orderID).First(&p).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// Create 创建一条支付流水。
func (r *repository) Create(p *Payment) error {
	return r.db.Create(p).Error
}

// UpdateStatus 更新某笔订单对应支付流水的状态。
func (r *repository) UpdateStatus(orderID uint, status string, paidAt *time.Time) error {
	return r.db.Model(&Payment{}).
		Where("order_id = ?", orderID).
		Updates(map[string]interface{}{
			"status":  status,
			"paid_at": paidAt,
		}).Error
}
