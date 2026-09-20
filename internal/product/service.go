package product

import "errors"

// ErrProductNotFound 表示商品不存在。
var ErrProductNotFound = errors.New("商品不存在")

// Service 是商品模块的业务层。
type Service struct {
	repo Repository
}

// NewService 创建商品业务层。
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create 创建商品。
func (s *Service) Create(name, description string, price float64) (*Product, error) {
	if name == "" {
		return nil, errors.New("商品名称不能为空")
	}
	if price < 0 {
		return nil, errors.New("价格不能为负数")
	}

	p := &Product{Name: name, Description: description, Price: price}
	if err := s.repo.Create(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Get 按 ID 查询商品。
func (s *Service) Get(id uint) (*Product, error) {
	p, err := s.repo.FindByID(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProductNotFound
	}
	return p, nil
}

// List 查询全部商品。
func (s *Service) List() ([]Product, error) {
	return s.repo.List()
}

// Delete 删除商品。
func (s *Service) Delete(id uint) error {
	p, err := s.repo.FindByID(id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrProductNotFound
	}
	return s.repo.Delete(id)
}

// CreateSKU 为商品创建一个规格。
func (s *Service) CreateSKU(productID uint, spec string, price float64) (*SKU, error) {
	if spec == "" {
		return nil, errors.New("规格不能为空")
	}
	if _, err := s.Get(productID); err != nil {
		return nil, err
	}

	sku := &SKU{ProductID: productID, Spec: spec, Price: price}
	if err := s.repo.CreateSKU(sku); err != nil {
		return nil, err
	}
	return sku, nil
}

// FindSKUByProductAndSpec 按「哪个商品 + 什么规格」查找一个 SKU。
// 找不到时返回 (nil, nil)，不是错误——「没有」也是一种正常的查询结果。
func (s *Service) FindSKUByProductAndSpec(productID uint, spec string) (*SKU, error) {
	return s.repo.FindSKUByProductAndSpec(productID, spec)
}

// ListSKUsByProductIDs 一次列出多个商品的全部规格。
// 前台列表页要同时展示十几个商品，用批量查询代替「一个商品查一次」。
func (s *Service) ListSKUsByProductIDs(productIDs []uint) ([]SKU, error) {
	return s.repo.ListSKUsByProductIDs(productIDs)
}
