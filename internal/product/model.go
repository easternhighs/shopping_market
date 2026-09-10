package product

import "time"

// Product 对应数据库里的 products 表，表示一个商品。
type Product struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"type:varchar(191);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	Price       float64   `json:"price"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SKU 对应数据库里的 skus 表，表示商品的一个具体规格。
// 例如“iPhone”是商品，“红色 128G”是它的一个 SKU。
type SKU struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	ProductID uint      `gorm:"index;not null" json:"product_id"`
	Spec      string    `gorm:"type:varchar(191);not null" json:"spec"`
	Price     float64   `json:"price"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
