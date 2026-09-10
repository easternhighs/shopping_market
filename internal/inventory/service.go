package inventory

import "errors"

// ErrStockNotFound 表示库存记录不存在。
var ErrStockNotFound = errors.New("库存记录不存在")

// Service 是库存模块的业务层。
type Service struct {
	repo Repository
}

// NewService 创建库存业务层。
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create 为某个 SKU 创建库存记录。
func (s *Service) Create(skuID uint, quantity int) (*Stock, error) {
	if quantity < 0 {
		return nil, errors.New("库存数量不能为负数")
	}

	existing, err := s.repo.FindBySKUID(skuID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, errors.New("该 SKU 已有库存记录")
	}

	stock := &Stock{SKUID: skuID, Quantity: quantity}
	if err := s.repo.Create(stock); err != nil {
		return nil, err
	}
	return stock, nil
}

// Get 查询 SKU 的库存。
func (s *Service) Get(skuID uint) (*Stock, error) {
	stock, err := s.repo.FindBySKUID(skuID)
	if err != nil {
		return nil, err
	}
	if stock == nil {
		return nil, ErrStockNotFound
	}
	return stock, nil
}

// Deduct 扣减库存，成功后返回最新库存。
func (s *Service) Deduct(skuID uint, quantity int) (*Stock, error) {
	if quantity <= 0 {
		return nil, errors.New("扣减数量必须大于 0")
	}
	if err := s.repo.Deduct(skuID, quantity); err != nil {
		return nil, err
	}
	return s.Get(skuID)
}
