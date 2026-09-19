package seckill

import (
	"context"
	"log"
	"time"
)

// ReconcileItem 对账一个秒杀商品：以 MySQL 的 sold 为准，修正 Redis 剩余库存。
//
// 为什么需要它：正常情况下 Redis 先扣、MySQL 后扣；如果中间失败或重复消费，
// 两边数字可能不一致。对账就是“拿数据库账本重新算一遍，再覆盖回 Redis”。
func (s *Service) ReconcileItem(ctx context.Context, itemID uint) error {
	// 动手改 Redis 之前，先确认此刻队列里没有订单在途中。
	//
	// 原因是这个函数拿 MySQL 的 sold 反推库存（剩余 = 总库存 - 已售）。
	// 只要还有订单在途中（Redis 已经预扣、MySQL 还没记账），sold 就是偏小的，
	// 反推出来的剩余库存就是偏大的；把这个偏大的数字写回 Redis，
	// 等于把已经卖掉的库存又变出来一次，高并发下就会超卖。
	// 判断依据见 Queue.Settled 的注释。
	if !s.queue.Settled() {
		return nil
	}

	item, err := s.repo.FindItemByID(itemID)
	if err != nil {
		return err
	}

	if item == nil {
		return ErrItemNotFound
	}

	realStock := item.TotalStock - item.Sold

	if err = s.cache.SetStock(ctx, item.ID, realStock); err != nil {
		return err
	}

	return nil
}

// ReconcileLoop 定期执行对账任务。
//
// 你可以把它理解成超市打烊后盘点库存的工作人员：每隔一段时间，把所有秒杀商品
// 的 Redis 库存和 MySQL 库存对齐一次。
func (s *Service) ReconcileLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			// 队列还没安静就整轮跳过：宁可晚几十秒再对账，
			// 也不能把在途订单的库存凭空加回去。
			if !s.queue.Settled() {
				log.Print("秒杀对账跳过本轮：仍有订单在途中，等待消费端追平")
				continue
			}

			items, err := s.repo.ListItems()
			if err != nil {
				// 这里以前写的是 return，等于一次数据库抖动就永久关掉了对账任务。
				// 对账是兜底手段，兜底自己不能因为一次失败就退场，所以记日志后等下一轮。
				log.Printf("秒杀对账查询商品列表失败，本轮跳过: %v", err)
				continue
			}

			for _, item := range items {
				err = s.ReconcileItem(ctx, item.ID)
				if err != nil {
					continue
				}
			}

		}

	}
}
