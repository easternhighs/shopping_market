package product

import "gorm.io/gorm"

// Repository 定义商品数据的读写接口。
type Repository interface {
	Create(p *Product) error
	FindByID(id uint) (*Product, error)
	List() ([]Product, error)
	CreateSKU(s *SKU) error
	Delete(id uint) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建商品仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// Create 保存一个新商品。
func (r *repository) Create(p *Product) error {
	return r.db.Create(p).Error
}

// FindByID 按 ID 查询商品。
func (r *repository) FindByID(id uint) (*Product, error) {
	var product Product

	if err := r.db.First(&product, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}

	return &product, nil
}

// List 查询全部商品。
func (r *repository) List() ([]Product, error) {
	// 声明一个 products 切片，用于存储多个商品记录
	var products []Product

	if err := r.db.Find(&products).Error; err != nil {
		return nil, err
	}

	return products, nil

}

// CreateSKU 保存一个商品规格。
func (r *repository) CreateSKU(s *SKU) error {
	return r.db.Create(s).Error
}

// Delete 按 ID 删除商品。
func (r *repository) Delete(id uint) error {
	return r.db.Delete(&Product{}, id).Error
}
