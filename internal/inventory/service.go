package inventory

import (
	"errors"
	"strings"
)

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

// DeductBatch 幂等地扣一批库存，供订单服务跨进程调用（阶段 16）。
//
// 它和上面的 Deduct 的区别：
//   - Deduct 是「扣一个 SKU」，给单体自己的 /stocks/:sku_id/deduct 用，没人重试它；
//   - DeductBatch 是「一单一起扣」，带一个 request_id 当幂等键。
//     订单服务失败会重试，重试时带的还是同一个 request_id，
//     所以这个方法重复调用一百次，库存也只扣第一次那一次。
func (s *Service) DeductBatch(requestID string, items []DeductItem) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return errors.New("request_id 不能为空")
	}
	if len(items) == 0 {
		return errors.New("items 不能为空")
	}
	for _, item := range items {
		if item.Quantity <= 0 {
			return errors.New("扣减数量必须大于 0")
		}
	}

	return s.repo.ApplyDeduct(requestID, items)
}
