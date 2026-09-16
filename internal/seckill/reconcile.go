package seckill

import (
	"context"
	"time"
)

// ReconcileItem 对账一个秒杀商品：以 MySQL 的 sold 为准，修正 Redis 剩余库存。
//
// 为什么需要它：正常情况下 Redis 先扣、MySQL 后扣；如果中间失败或重复消费，
// 两边数字可能不一致。对账就是“拿数据库账本重新算一遍，再覆盖回 Redis”。
func (s *Service) ReconcileItem(ctx context.Context, itemID uint) error {
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
			items, err := s.repo.ListItems()
			if err != nil {
				return
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
