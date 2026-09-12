package order

import (
	"errors"
	"strings"

	"shopping_market/internal/inventory"

	"gorm.io/gorm"
)

// ErrOrderNotFound 表示订单不存在。
var ErrOrderNotFound = errors.New("订单不存在")

// ErrInvalidOrderStatus 表示订单当前状态不允许这次操作。
var ErrInvalidOrderStatus = errors.New("订单状态不允许该操作")

// PlaceItem 是下单请求里的一个商品明细。
type PlaceItem struct {
	SKUID    uint `json:"sku_id"`
	Quantity int  `json:"quantity"`
}

// Service 是订单模块的业务层。
type Service struct {
	repo      Repository
	inventory inventory.Repository
}

// NewService 创建订单业务层。
func NewService(repo Repository, inventory inventory.Repository) *Service {
	return &Service{repo: repo, inventory: inventory}
}

// Place 创建订单。
// TODO(你来实现)：完成“幂等检查 + 事务下单”。
// 流程：
//  1. 参数校验：requestID 和 items 不能为空，每个 quantity 大于 0；
//  2. 幂等检查：用 repo.FindByRequestID 查询，已存在就直接返回该订单；
//  3. 用 repo.Transaction 开启事务，在事务里：
//     a. 遍历 items，调用 inventory.DeductWithDB(tx, item.SKUID, item.Quantity) 扣库存；
//     b. 创建订单（RequestID、UserID、Status 用 "pending"）；
//     c. 把 items 转成 []Item 并创建明细。
func (s *Service) Place(userID uint, requestID string, items []PlaceItem) (*Order, error) {
	requestID = strings.TrimSpace(requestID)

	//1.检验参数：requestID 和 items 不能为空，每个 quantity 大于 0
	if requestID == "" || len(items) == 0 {
		return nil, errors.New("requestID 和 items 不能为空")
	}

	for _, item := range items {
		if item.Quantity <= 0 {
			return nil, errors.New("quantity 必须大于 0")
		}
	}

	//2.幂等检查：用 repo.FindByRequestID 查询，已存在就直接返回该订单
	o, err := s.repo.FindByRequestID(requestID)
	if err != nil {
		return nil, err
	}
	//应该改为o存在时直接返回，否则第一次创建订单时就会报错
	if o != nil {
		return o, nil
	}

	//3.用 repo.Transaction 开启事务
	var createdOrder *Order

	if err := s.repo.Transaction(func(tx *gorm.DB) error {
		//3.在事务里：
		// a.遍历 items，调用 inventory.DeductWithDB(tx, item.SKUID, item.Quantity) 扣库存；
		for _, item := range items {
			if err := s.inventory.DeductWithDB(tx, item.SKUID, item.Quantity); err != nil {
				return err
			}
		}

		// b.创建订单（RequestID、UserID、Status 用 "pending"）；
		order := &Order{
			RequestID: requestID,
			UserID:    userID,
			Status:    "pending",
		}
		if err := s.repo.Create(tx, order); err != nil {
			return err
		}
		createdOrder = order

		// c.把 items 转成 []Item 并创建明细。
		//注意处理顺序，由于order.ID是数据库自增生成的，因此需要先交给数据库创建订单，再创建明细

		orderItems := make([]Item, 0, len(items))
		for _, item := range items {
			orderItems = append(orderItems, Item{
				OrderID:  order.ID, //步骤3.b中创建订单后，会自动生成order.ID，因此这里可以直接使用
				SKUID:    item.SKUID,
				Quantity: item.Quantity,
			})
		}

		if err := s.repo.CreateItems(tx, orderItems); err != nil {
			return err
		}

		return nil

	}); err != nil {
		return nil, err
	}

	//若2中的订单不存在，则将3中的事务中创建的订单返回
	return createdOrder, nil

}

// Get 查询订单及明细。
func (s *Service) Get(id uint) (*Order, []Item, error) {
	o, err := s.repo.FindByID(id)
	if err != nil {
		return nil, nil, err
	}
	if o == nil {
		return nil, nil, ErrOrderNotFound
	}

	items, err := s.repo.FindItemsByOrderID(id)
	if err != nil {
		return nil, nil, err
	}
	return o, items, nil
}

// Pay 把订单状态改成 paid。
// TODO(你来实现)：完成“订单支付状态流转”：
//  1. 查询订单，订单不存在返回 ErrOrderNotFound；
//  2. 只有 pending 状态的订单才能支付，其他状态返回错误；
//  3. 调用 repo.UpdateStatus 把状态改成 paid。
func (s *Service) Pay(orderID uint) error {
	o, err := s.repo.FindByID(orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrOrderNotFound
	}

	if o.Status != "pending" {
		return ErrInvalidOrderStatus
	}

	if err := s.repo.UpdateStatus(orderID, "paid"); err != nil {
		return err
	}

	return nil
}

// Cancel 把订单状态改成 cancelled。
// TODO(你来实现)：完成“订单取消状态流转”：
//  1. 查询订单，订单不存在返回 ErrOrderNotFound；
//  2. 只有 pending 状态的订单才能取消，其他状态返回错误；
//  3. 调用 repo.UpdateStatus 把状态改成 cancelled。
func (s *Service) Cancel(orderID uint) error {
	o, err := s.repo.FindByID(orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrOrderNotFound
	}

	if o.Status != "pending" {
		return ErrInvalidOrderStatus
	}

	if err := s.repo.UpdateStatus(orderID, "cancelled"); err != nil {
		return err
	}

	return nil
}
