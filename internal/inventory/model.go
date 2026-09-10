package inventory

import "time"

// Stock 对应数据库里的 stocks 表，表示某个 SKU 的库存数量。
type Stock struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SKUID     uint      `gorm:"column:sku_id;uniqueIndex;not null" json:"sku_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
