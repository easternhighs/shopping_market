package seckill

import (
	"context"
	"errors"
	"log"
	"sync"

	kafkaclient "shopping_market/internal/pkg/kafka"

	"gorm.io/gorm"
)

var (
	ErrRequestExists = errors.New("请求已经存在")
	ErrOrderExists   = errors.New("订单已经存在")
)

// consumeBatchSize 是一个消费者一次从 Kafka 拉多少条消息。
// 批量太小省不下网络往返；太大会让“一批要处理很久才提交”，中途崩溃时重做的活更多。
const consumeBatchSize = 200

// ConsumeOrders 持续从 Kafka 读取秒杀消息，并把成功预扣的请求最终写入 MySQL。
//
// 你可以把它理解成“收银台后面的工作人员”：HTTP 接口只负责让用户先排队，
// 这里才是真正把订单和库存写进数据库的地方。
//
// 为什么现在要开多个 goroutine：一个消费者一次只能顺序处理一条条消息，
// 处理速度顶天几百条/秒，活动一结束就会堆积一大堆消息。
// 现在让每个 goroutine 独占一个消费者，Kafka 会把不同的分区分给不同的消费者，
// 于是多个 goroutine 就能同时处理不同分区的消息，吞吐成倍上涨。
func (s *Service) ConsumeOrders(ctx context.Context) error {
	consumers := s.queue.Consumers()
	if len(consumers) == 0 {
		return errors.New("没有可用的 Kafka 消费者")
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(consumers))

	for _, consumer := range consumers {
		wg.Add(1)
		go func(consumer *kafkaclient.Consumer) {
			defer wg.Done()
			if err := s.consumeLoop(ctx, consumer); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}(consumer)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return nil
}

// consumeLoop 是单个消费者的工作循环：批量拉取 → 整批落库 → 批量提交消费进度。
//
// 和上一版“逐条落库”的区别只有一处，但很关键：现在整批消息只开一个 MySQL 事务，
// 于是同一行库存只被 UPDATE 一次、行锁只抢一次，多个消费者不再排队等锁。
//
// 有一个取舍要记住：批量提交意味着失败的消息不会自动重投，
// 所以落库失败时必须老老实实调用 compensate 把 Redis 预扣的库存还回去；
// 万一补偿也失败，最后的库存正确性由对账任务兜底。
func (s *Service) consumeLoop(ctx context.Context, consumer *kafkaclient.Consumer) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		consumedOrders, err := s.queue.ConsumeBatch(ctx, consumer, consumeBatchSize)
		if err != nil {
			// 这里是“真错误”（网络、权限等）。返回给上层，让它记日志/告警；
			// 这个消费者 goroutine 随之退出，Kafka 会把它的分区重新分给别的消费者。
			return err
		}
		if len(consumedOrders) == 0 {
			continue
		}

		// 先把 JSON 解析失败的消息挑出去：它不参与业务处理，
		// 但仍然留在 consumedOrders 里跟着这一批一起提交，
		// 否则一条坏消息会把消费进度永远卡住。
		msgs := make([]OrderMessage, 0, len(consumedOrders))
		for _, consumedOrder := range consumedOrders {
			if consumedOrder.Err != nil {
				log.Print("当前订单出现错误", consumedOrder.Err)
				continue
			}
			msgs = append(msgs, consumedOrder.Order)
		}

		// 整批消息只开一个事务落库，具体实现在 processOrdersBatch 里。
		result := s.processOrdersBatch(ctx, msgs)

		for range result.Success {
			s.observeSeckillResult("success")
		}

		// 重复消息不回补 Redis：所谓“重复”，是 MySQL 里已经有这条订单，
		// 说明当初那次扣减是真实成交的；这里再回补一次，Redis 库存就会凭空多出来。
		for range result.Duplicates {
			s.observeSeckillResult("duplicate")
		}

		// 没写进 MySQL 的消息才需要回补：把 Redis 预扣的库存和购买数还回去。
		for _, msg := range result.Failed {
			if errComp := s.compensate(ctx, msg); errComp != nil {
				// 补偿失败意味着这条消息只能等 30 秒一次的对账任务来纠正，
				// 先记下来，不能因为一条消息把整个消费循环停掉。
				log.Print("秒杀补偿失败，等待对账兜底", errComp)
			}
			s.observeSeckillResult("failure")
		}

		if err = s.queue.Commit(ctx, consumer, consumedOrders); err != nil {
			return err
		}

	}

}

// processOrder 把一条 Kafka 消息落库（单条版）。
//
// 这是秒杀“最后一道防线”：Redis 已经在入口预扣过一次，这里再用 MySQL
// 的条件更新扣一次真实库存，并创建订单。
//
// 注意：消费循环现在走的是批量版 processOrdersBatch，这个单条版暂时保留，
// 用来对照和排查问题；等批量版稳定后可以删掉。
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
