package cart

import "errors"

// Service 是购物车模块的业务层。
type Service struct {
	repo Repository
}

// NewService 创建购物车业务层。
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// AddItem 往用户购物车里加一个 SKU。
func (s *Service) AddItem(userID, skuID uint, quantity int) (*Item, error) {
	//拒绝添加数量小于等于0的商品
	if quantity <= 0 {
		return nil, errors.New("数量必须大于 0")
	}

	//1.查找用户的购物车,没有则进行创建
	cart, err := s.repo.FindCartByUserID(userID)
	if err != nil {
		return nil, err
	}

	if cart == nil {
		// 如果没有购物车，就创建一个新的购物车
		cart = &Cart{UserID: userID}
		if err := s.repo.CreateCart(cart); err != nil {
			return nil, err
		}
	}

	//2.查找购物车里是否已有这个 SKU，若没有则新增一条记录，若有则进行叠加。
	FoundSKU, err := s.repo.FindItem(cart.ID, skuID)
	if err != nil {
		return nil, err
	}

	if FoundSKU != nil {
		// 如果购物车里已有这个 SKU，就把数量加上去
		FoundSKU.Quantity += quantity
		if err := s.repo.UpdateItemQuantity(FoundSKU.ID, FoundSKU.Quantity); err != nil {
			return nil, err
		}
		return FoundSKU, nil
	}

	//3.如果购物车里没有这个 SKU，就新增一条明细
	item := &Item{
		CartID:   cart.ID,
		SKUID:    skuID,
		Quantity: quantity,
	}

	//新增购物车明细
	return item, s.repo.AddItem(item)

}

// List 查询用户购物车的全部明细。
func (s *Service) List(userID uint) ([]Item, error) {
	cart, err := s.repo.FindCartByUserID(userID)
	if err != nil {
		return nil, err
	}
	if cart == nil {
		return []Item{}, nil
	}
	return s.repo.ListItems(cart.ID)
}

// UpdateQuantity 更新明细数量。
func (s *Service) UpdateQuantity(itemID uint, quantity int) error {
	if quantity <= 0 {
		return errors.New("数量必须大于 0")
	}
	return s.repo.UpdateItemQuantity(itemID, quantity)
}

// RemoveItem 删除明细。
func (s *Service) RemoveItem(itemID uint) error {
	return s.repo.DeleteItem(itemID)
}
