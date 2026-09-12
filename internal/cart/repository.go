package cart

import "gorm.io/gorm"

// Repository 定义购物车数据的读写接口。
type Repository interface {
	FindCartByUserID(userID uint) (*Cart, error)
	CreateCart(c *Cart) error
	FindItem(cartID, skuID uint) (*Item, error)
	AddItem(item *Item) error
	UpdateItemQuantity(itemID uint, quantity int) error
	ListItems(cartID uint) ([]Item, error)
	DeleteItem(itemID uint) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建购物车仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindCartByUserID 按用户 ID 查找购物车。
func (r *repository) FindCartByUserID(userID uint) (*Cart, error) {
	var cart Cart
	if err := r.db.Where("user_id = ?", userID).First(&cart).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &cart, nil
}

// CreateCart 创建购物车。
func (r *repository) CreateCart(c *Cart) error {
	return r.db.Create(c).Error
}

// FindItem 查找购物车里某个 SKU 的明细。
func (r *repository) FindItem(cartID, skuID uint) (*Item, error) {
	var item Item
	if err := r.db.Where("cart_id = ? AND sku_id = ?", cartID, skuID).First(&item).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

// AddItem 新增购物车明细。
func (r *repository) AddItem(item *Item) error {
	return r.db.Create(item).Error
}

// UpdateItemQuantity 更新明细数量。
func (r *repository) UpdateItemQuantity(itemID uint, quantity int) error {
	return r.db.Model(&Item{}).Where("id = ?", itemID).Update("quantity", quantity).Error
}

// ListItems 查询购物车全部明细。
func (r *repository) ListItems(cartID uint) ([]Item, error) {
	var items []Item
	if err := r.db.Where("cart_id = ?", cartID).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// DeleteItem 删除购物车明细。
func (r *repository) DeleteItem(itemID uint) error {
	return r.db.Delete(&Item{}, itemID).Error
}
