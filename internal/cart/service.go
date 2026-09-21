package cart

import "errors"

// ErrItemNotOwned 表示这条购物车明细不属于当前用户，或者压根不存在。
//
// 为什么不给这个错误起名叫「这不是你的明细」：那样等于告诉攻击者「这个 id 是存在的，
// 只是不归你」——光这一句就是一条有用的情报，他可以把 id 从小到大试一遍，
// 数出别人有多少条明细。所以对外统一说「不存在」。
var ErrItemNotOwned = errors.New("购物车明细不存在")

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
//
// 越权（IDOR）防护：函数不能只认 itemID，还得认人。任何登录用户只要把 id 猜出来
// （就是 1、2、3 ……），就能改别人的购物车 —— 前端页面看不到别人的明细 ID，
// 但攻击者不需要页面，他拿自己的令牌直接 curl 打接口就行。
//
// 所以这里先问一句「这条明细是不是这个用户的」，不是就直接返回 ErrItemNotOwned，
// 后面的更新一行都不执行。
//
// 为什么归属判断要写进 SQL（仓储里那个 JOIN），而不是「先查出来再比较 item.CartID」：
// 一条 SQL 就够，省一次数据库往返；更重要的是，条件写在查询里，调用方就没有「忘了比较」的机会。
func (s *Service) UpdateQuantity(userID, itemID uint, quantity int) error {
	if quantity <= 0 {
		return errors.New("数量必须大于 0")
	}

	owned, err := s.repo.FindItemByIDForUser(itemID, userID)
	if err != nil {
		return err
	}
	if owned == nil {
		return ErrItemNotOwned
	}

	return s.repo.UpdateItemQuantity(itemID, quantity)
}

// RemoveItem 删除明细。
//
// 和 UpdateQuantity 一样，先做归属校验，通过了再调用 s.repo.DeleteItem(itemID)。
// 删除比修改更危险：改错了还能改回来，删掉就是真的没了。
func (s *Service) RemoveItem(userID, itemID uint) error {
	owned, err := s.repo.FindItemByIDForUser(itemID, userID)
	if err != nil {
		return err
	}
	if owned == nil {
		return ErrItemNotOwned
	}

	return s.repo.DeleteItem(itemID)
}
