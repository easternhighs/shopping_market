package payment

import (
	"errors"
	"time"

	"shopping_market/internal/order"
)

// ErrPaymentNotFound 表示没有找到支付流水。
var ErrPaymentNotFound = errors.New("支付流水不存在")

// Service 是支付模块的业务层。
// 它依赖订单模块，因为支付成功后要通知订单模块把状态改成 paid。
type Service struct {
	repo   Repository
	orders *order.Service
}

// NewService 创建支付业务层。
func NewService(repo Repository, orders *order.Service) *Service {
	return &Service{repo: repo, orders: orders}
}

// Callback 模拟第三方支付成功后的回调。
// TODO(你来实现)：完成下面的支付回调流程：
//  1. 先确认订单存在，并且属于当前登录用户；
//  2. 按 orderID 查询支付流水，没有就先创建一条 pending 流水；
//  3. 如果流水已经是 paid，直接返回，保证重复回调幂等；
//  4. 调用 orders.Pay(orderID) 把订单状态改成 paid；
//  5. 把支付流水更新成 paid，并记录 PaidAt 时间；
//  6. 如果订单不存在、不属于当前用户或状态不允许支付，返回对应错误。
func (s *Service) Callback(userID, orderID uint) (*Payment, error) {

	//1.先确认订单存在，并且属于当前登录用户
	o, _, err := s.orders.Get(orderID)
	if err != nil {
		return nil, err
	}
	if o.UserID != userID {
		return nil, errors.New("订单不属于当前用户")
	}

	//2.按 orderID 查询支付流水，没有就先创建一条 pending 流水
	payment, err := s.repo.FindByOrderID(orderID)
	if err != nil {
		return nil, err
	}

	if payment == nil {
		payment = &Payment{
			OrderID: orderID,
			Status:  "pending",
		}
		err = s.repo.Create(payment)
		if err != nil {
			return nil, err
		}
	}

	//3.如果流水已经是 paid，直接返回，保证重复回调幂等
	if payment.Status == "paid" {
		return payment, nil
	}

	//4.调用 orders.Pay(orderID) 把订单状态改成 paid
	err = s.orders.Pay(orderID)
	if err != nil {
		return nil, err
	}

	//5.把支付流水更新成 paid，并记录 PaidAt 时间
	currentTime := time.Now()

	payment.Status = "paid"
	payment.PaidAt = &currentTime
	//UpdateStatus 方法会在数据库中更新支付流水的状态和支付时间
	err = s.repo.UpdateStatus(orderID, "paid", &currentTime)
	if err != nil {
		return nil, err
	}

	return payment, nil
}

// Get 查询某笔订单的支付流水。
func (s *Service) Get(orderID uint) (*Payment, error) {
	p, err := s.repo.FindByOrderID(orderID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrPaymentNotFound
	}
	return p, nil
}
