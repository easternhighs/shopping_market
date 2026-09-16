package seckill

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

var (
	ErrRequestExists = errors.New("请求已经存在")
	ErrOrderExists   = errors.New("订单已经存在")
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
			// MySQL 事务已经回滚，这里把 Redis 预扣的部分还回去。
			if rollbackErr := s.compensate(ctx, msg); rollbackErr != nil {
				s.observeSeckillResult("failure")
				return rollbackErr
			}
			if errors.Is(err, ErrOrderExists) || errors.Is(err, ErrRequestExists) {
				s.observeSeckillResult("duplicate")
				continue
			}

			s.observeSeckillResult("failure")
			return err
		}

		s.observeSeckillResult("success")
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
			return ErrRequestExists
		}

		existingUserOrder, err := s.repo.FindOrderByUserAndItemWithTx(tx, msg.UserID, msg.ItemID)
		if err != nil {
			return err
		}
		if existingUserOrder != nil {
			return ErrOrderExists
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

// compensate 在消息处理失败后回补 Redis。
// 它不负责判断要不要回补，只负责把 Redis 库存和用户购买数还回去。
func (s *Service) compensate(ctx context.Context, msg OrderMessage) error {
	return s.cache.RollbackDeduct(ctx, msg.ItemID, msg.UserID, msg.Quantity)
}
