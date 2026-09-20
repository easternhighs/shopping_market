package cart

import "gorm.io/gorm"

// Repository 定义购物车数据的读写接口。
type Repository interface {
	FindCartByUserID(userID uint) (*Cart, error)
	CreateCart(c *Cart) error
	FindItem(cartID, skuID uint) (*Item, error)
	// FindItemByIDForUser 按「明细 ID + 用户 ID」查一条明细。
	//
	// 为什么要带上 userID：只按 id 查的话，任何登录用户猜到 id 就能碰到别人的购物车
	// （这叫越权 / IDOR）。把归属条件直接写进查询里，调用方就没机会「忘了比较」。
	// 查不到时返回 (nil, nil) —— 「不存在」和「不属于你」在这里不做区分，
	// 免得给攻击者提供「这个 id 存在」的情报。
	FindItemByIDForUser(itemID, userID uint) (*Item, error)
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

// FindItemByIDForUser 用一次 JOIN 把「这条明细属不属于这个用户」和「取出明细」一起做掉。
// 写成 JOIN 而不是「先查明细、再用 cart_id 反查购物车」，是为了省一次数据库往返：
// 后者在购物车这种每点一下都要调的接口上，等于把响应时间白翻一倍。
func (r *repository) FindItemByIDForUser(itemID, userID uint) (*Item, error) {
	var item Item
	if err := r.db.Table("cart_items").
		Joins("JOIN carts ON carts.id = cart_items.cart_id").
		Where("cart_items.id = ? AND carts.user_id = ?", itemID, userID).
		First(&item).Error; err != nil {
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
