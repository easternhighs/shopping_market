package order

import "time"

// Order 对应数据库里的 orders 表，表示一个订单。
type Order struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RequestID string    `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	UserID    uint      `gorm:"column:user_id;index;not null" json:"user_id"`
	Status    string    `gorm:"type:varchar(32);not null;default:'pending'" json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 对应数据库里的 order_items 表，表示订单里的一个商品明细。
type Item struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	OrderID   uint      `gorm:"column:order_id;index;not null" json:"order_id"`
	SKUID     uint      `gorm:"column:sku_id;not null" json:"sku_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 Item 对应的表名为 order_items。
func (Item) TableName() string {
	return "order_items"
}
