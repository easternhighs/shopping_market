package inventory

import (
	"errors"

	"gorm.io/gorm"
)

// ErrInsufficientStock 表示库存不足。
var ErrInsufficientStock = errors.New("库存不足")

// Repository 定义库存数据的读写接口。
type Repository interface {
	FindBySKUID(skuID uint) (*Stock, error)
	Deduct(skuID uint, quantity int) error
	Create(s *Stock) error
	DeductWithDB(db *gorm.DB, skuID uint, quantity int) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建库存仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindBySKUID 按 SKU ID 查询库存记录。
func (r *repository) FindBySKUID(skuID uint) (*Stock, error) {
	var stock Stock
	if err := r.db.Where("sku_id = ?", skuID).First(&stock).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &stock, nil
}

// Deduct 扣减库存数量。
// 这是以后“防超卖”的关键函数，必须保证“库存够才扣”。
// TODO(你来实现)：
//  1. 用条件更新：库存数量减去 quantity，条件是 sku_id 匹配且 quantity >= 要扣的数量；
//  2. 如果受影响行数为 0，说明库存不足，返回一个错误；
//  3. 其他数据库错误也要返回。
func (r *repository) Deduct(skuID uint, quantity int) error {
	stock, err := r.FindBySKUID(skuID)
	if err != nil {
		return err
	}
	if stock == nil {
		return ErrInsufficientStock
	}

	// 操作对应SKUID的库存记录，只有数量>=要扣除的数量时，则进行扣减操作
	result := r.db.Model(&stock).
		Where("sku_id = ? AND quantity >= ?", skuID, quantity).
		Update("quantity", gorm.Expr("quantity - ?", quantity))

	//先判断数据库是否报错，再判断Update是否有受影响的行数，如果没有则说明库存不足，返回库存不足的错误
	if result.Error != nil {
		return result.Error
	}

	//判断当前Update操作是否有受影响的行数，如果没有则说明库存不足，返回库存不足的错误
	if result.RowsAffected == 0 {
		return ErrInsufficientStock
	}

	return nil
}

// Create 创建一条库存记录。
func (r *repository) Create(s *Stock) error {
	return r.db.Create(s).Error
}

// DeductWithDB 在指定事务里扣减库存，供订单等需要“同事务”的场景使用。
func (r *repository) DeductWithDB(db *gorm.DB, skuID uint, quantity int) error {
	result := db.Model(&Stock{}).
		Where("sku_id = ? AND quantity >= ?", skuID, quantity).
		Update("quantity", gorm.Expr("quantity - ?", quantity))

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInsufficientStock
	}
	return nil
}
