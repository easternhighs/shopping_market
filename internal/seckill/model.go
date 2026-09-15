package seckill

import "time"

// Activity 对应数据库里的 seckill_activities 表，表示一场秒杀活动。
type Activity struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"type:varchar(191);not null" json:"name"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 对应数据库里的 seckill_items 表，表示这场活动里的一个秒杀商品。
type Item struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	ActivityID   uint      `gorm:"column:activity_id;uniqueIndex:idx_activity_sku;not null" json:"activity_id"`
	SKUID        uint      `gorm:"column:sku_id;uniqueIndex:idx_activity_sku;not null" json:"sku_id"`
	Price        float64   `json:"price"`
	TotalStock   int       `gorm:"not null" json:"total_stock"`
	Sold         int       `gorm:"not null;default:0" json:"sold"`
	PerUserLimit int       `gorm:"not null;default:1" json:"per_user_limit"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Order 对应数据库里的 seckill_orders 表，表示一次秒杀下单记录。
type Order struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	RequestID  string    `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	ActivityID uint      `gorm:"column:activity_id;index;not null" json:"activity_id"`
	ItemID     uint      `gorm:"column:item_id;uniqueIndex:idx_user_item;not null" json:"item_id"`
	SKUID      uint      `gorm:"column:sku_id;not null" json:"sku_id"`
	UserID     uint      `gorm:"column:user_id;uniqueIndex:idx_user_item;index;not null" json:"user_id"`
	Quantity   int       `gorm:"not null" json:"quantity"`
	Status     string    `gorm:"type:varchar(32);not null;default:'pending'" json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TableName 指定 Activity 对应的表名。
func (Activity) TableName() string {
	return "seckill_activities"
}

// TableName 指定 Item 对应的表名。
func (Item) TableName() string {
	return "seckill_items"
}

// TableName 指定 Order 对应的表名。
func (Order) TableName() string {
	return "seckill_orders"
}
