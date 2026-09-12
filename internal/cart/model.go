package cart

import "time"

// Cart 对应数据库里的 carts 表，表示一个用户的购物车。
type Cart struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"column:user_id;uniqueIndex;not null" json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 对应数据库里的 cart_items 表，表示购物车里的一个商品明细。
type Item struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CartID    uint      `gorm:"column:cart_id;uniqueIndex:idx_cart_sku;not null" json:"cart_id"`
	SKUID     uint      `gorm:"column:sku_id;uniqueIndex:idx_cart_sku;not null" json:"sku_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 Item 对应的表名为 cart_items。
func (Item) TableName() string {
	return "cart_items"
}
