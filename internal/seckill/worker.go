package seckill

import (
	"context"

	"gorm.io/gorm"
)

// ConsumeOrders 持续从 Kafka 读取秒杀消息，并把成功预扣的请求最终写入 MySQL。
//
// 你可以把它理解成“收银台后面的工作人员”：HTTP 接口只负责让用户先排队，
// 这里才是真正把订单和库存写进数据库的地方。
func (s *Service) ConsumeOrders(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := s.queue.Consume(ctx)
		if err != nil {
			return err
		}

		if err := s.processOrder(ctx, msg); err != nil {
			return err
		}
	}
}

// processOrder 把一条 Kafka 消息落库。
//
// 这是秒杀“最后一道防线”：Redis 已经在入口预扣过一次，这里再用 MySQL
// 的条件更新扣一次真实库存，并创建订单。
func (s *Service) processOrder(ctx context.Context, msg OrderMessage) error {
	if err := s.repo.Transaction(func(tx *gorm.DB) error {
		existingRequest, err := s.repo.FindOrderByRequestIDWithTx(tx, msg.RequestID)
		if err != nil {
			return err
		}
		if existingRequest != nil {
			return nil
		}

		existingUserOrder, err := s.repo.FindOrderByUserAndItemWithTx(tx, msg.UserID, msg.ItemID)
		if err != nil {
			return err
		}
		if existingUserOrder != nil {
			return nil
		}

		if err := s.repo.DeductStock(tx, msg.ItemID, msg.Quantity); err != nil {
			return err
		}

		order := &Order{
			RequestID:  msg.RequestID,
			ActivityID: msg.ActivityID,
			ItemID:     msg.ItemID,
			SKUID:      msg.SKUID,
			UserID:     msg.UserID,
			Quantity:   msg.Quantity,
			Status:     "pending",
		}

		return s.repo.CreateOrderWithTx(tx, order)
	}); err != nil {
		return err
	}

	return nil
}
