package payment

import "time"

// Payment 对应数据库里的 payments 表，表示一次支付流水。
// 这里只是模拟支付，所以不需要真实金额、支付渠道等复杂字段。
type Payment struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	OrderID   uint       `gorm:"column:order_id;uniqueIndex;not null" json:"order_id"`
	Status    string     `gorm:"type:varchar(32);not null;default:'pending'" json:"status"`
	PaidAt    *time.Time `json:"paid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
