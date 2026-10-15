package order

import (
	"context"
	"errors"
	"log"
	"time"
)

// RetryLoop 是「库存补偿任务」，一个跑在后台的死循环（阶段 16 新增）。
//
// 它要做的事，用一句话说就是：**不停地把那些「库存还没扣成功」的订单捡起来，再试一次。**
//
// 为什么必须有它：下单时那次跨进程调用可能失败（对方没起来、网络抖了一下）。
// 失败之后如果没人管，订单就会永远卡在「库存待确认」——用户以为下单成功了，
// 库存却一直没扣，最后变成超卖。这个循环就是「最终一致」里那个「最终」的执行者。
//
// 它每隔 interval 醒一次，每轮做三件事：
//  1. 找出这一轮该处理的订单（便签是 pending、到点了、没超过重试上限）；
//  2. 逐条「领取」——领到才处理，领不到说明别的任务正在处理（见 ClaimOutboxRetry）；
//  3. 按结果收尾：成功 → 订单变 pending + 便签划掉；
//     库存真的不够 → 关单 + 便签判死；
//     其它失败 → 记下次数、按退避时间约下一次。
//
// ctx 一取消就退出，这样进程收到 Ctrl+C 时不会拖着这条 goroutine 不放。
func (s *Service) RetryLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = RetryInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[订单] 库存补偿任务已启动：每 %s 扫一次，每条最多重试 %d 次", interval, OutboxMaxAttempts)

	for {
		select {
		case <-ctx.Done():
			log.Println("[订单] 库存补偿任务退出")
			return
		case <-ticker.C:
			s.retryOnce(ctx)
		}
	}
}

// retryOnce 是补偿任务的一轮：抓一批该处理的订单，逐条处理。
//
// 一轮里任何一条出问题都不能影响其他条，所以错误只在 retryOne 内部记日志、
// 不往外抛——后台任务是「跑得久」比「跑得快」重要，绝不能因为一条烂数据整个循环退出。
func (s *Service) retryOnce(ctx context.Context) {
	now := time.Now()

	pendings, err := s.repo.ListRetryableOrders(RetryBatchSize, OutboxMaxAttempts, now)
	if err != nil {
		log.Printf("[订单] 查询待补偿订单失败: %v", err)
		return
	}

	for _, pending := range pendings {
		s.retryOne(ctx, pending)
	}
}

// retryOne 补偿一张订单。
func (s *Service) retryOne(ctx context.Context, pending RetryableOrder) {
	requestID := pending.Outbox.RequestID

	// 先「领任务」再干活。领不到（nil）说明别人已经领走了，直接跳过。
	rec, err := s.repo.ClaimOutboxRetry(requestID, OutboxLease, time.Now())
	if err != nil {
		log.Printf("[订单] 领取补偿任务失败: request_id=%s err=%v", requestID, err)
		return
	}
	if rec == nil {
		return
	}

	// 领到之后再确认一次订单状态：从「上一轮失败」到现在这段时间里，
	// 用户完全可能已经把这单取消了。已经取消的单子再去扣库存，
	// 就变成「库存扣了、订单没了」，所以这里必须重新看一眼。
	// 注意：这一步和上面那步不是一回事——上面看的是便签，这里看的是订单。
	fresh, err := s.repo.FindByID(pending.Order.ID)
	if err != nil {
		log.Printf("[订单] 补偿前查询订单失败: order=%d err=%v", pending.Order.ID, err)
		return
	}
	if fresh == nil || fresh.Status != StatusStockPending {
		// 订单已经不在这儿等着了（取消了 / 被别的路径处理掉了），便签一起作废。
		if err := s.repo.MarkOutboxCancelled(requestID, "订单已不在「库存待确认」状态，放弃补偿"); err != nil {
			log.Printf("[订单] 作废便签失败: request_id=%s err=%v", requestID, err)
		}
		return
	}

	items, err := unmarshalOutboxPayload(rec.Payload)
	if err != nil {
		// 消息体坏了：重试一万次也解析不出来，属于「修不好」的错，直接放弃。
		// 这种情况下把订单关掉，比让它永远挂着更诚实。
		s.giveUp(pending.Order.ID, requestID, rec.Attempts, "outbox 消息体无法解析: "+err.Error())
		return
	}

	// 真正去扣库存。同一个 request_id 重复调用的结果是「幂等」的：
	// 如果上一次其实扣成功了、只是我们没收到回执，这一次会直接回「已扣过」，不会被扣两遍。
	err = s.deduct.Deduct(ctx, requestID, fromOutbox(items))

	switch {
	case err == nil:
		if uerr := s.repo.UpdateStatus(pending.Order.ID, StatusPending); uerr != nil {
			log.Printf("[订单] 补偿成功但改状态失败，下一轮继续: order=%d err=%v", pending.Order.ID, uerr)
			return
		}
		if merr := s.repo.MarkOutboxDone(requestID); merr != nil {
			log.Printf("[订单] 补偿成功但便签标记失败: request_id=%s err=%v", requestID, merr)
		}
		log.Printf("[订单] 补偿成功: order=%d request_id=%s 已确认库存", pending.Order.ID, requestID)

	case errors.Is(err, ErrStockInsufficient):
		// 库存确实不够：等下去也不会变多（除非有人补货，但那是另一套逻辑）。
		// 关单，并把便签判死——留成 pending 的话它会一直占着一轮的名额。
		if uerr := s.repo.UpdateStatus(pending.Order.ID, StatusCancelled); uerr != nil {
			log.Printf("[订单] 库存不足关单失败: order=%d err=%v", pending.Order.ID, uerr)
		}
		if merr := s.repo.MarkOutboxDead(requestID, rec.Attempts+1, ErrStockInsufficient.Error()); merr != nil {
			log.Printf("[订单] 判死便签失败: request_id=%s err=%v", requestID, merr)
		}
		log.Printf("[订单] 补偿判定库存不足，已关单: order=%d request_id=%s", pending.Order.ID, requestID)

	default:
		attempts := rec.Attempts + 1
		if attempts >= OutboxMaxAttempts {
			s.giveUp(pending.Order.ID, requestID, attempts, err.Error())
			return
		}

		next := time.Now().Add(nextRetryDelay(attempts))
		if rerr := s.repo.RescheduleOutbox(requestID, attempts, next, err.Error()); rerr != nil {
			log.Printf("[订单] 登记下次重试失败: request_id=%s err=%v", requestID, rerr)
			return
		}
		log.Printf("[订单] 补偿失败（第 %d 次），%s 后再试: request_id=%s err=%v",
			attempts, nextRetryDelay(attempts), requestID, err)
	}
}

// giveUp 是补偿的终点：次数用光或者错误根本修不好，只能放弃这一单。
//
// 「放弃」不是「不管了」，而是要把状态收拾干净：
// 订单关掉（不能让用户一直看到「确认中」），便签判死（不能再占着重试名额），
// 失败原因留在 last_error 里（事后有人要查，就靠这一列）。
func (s *Service) giveUp(orderID uint, requestID string, attempts int, reason string) {
	if err := s.repo.UpdateStatus(orderID, StatusCancelled); err != nil {
		log.Printf("[订单] 放弃补偿时关单失败: order=%d err=%v", orderID, err)
	}
	if err := s.repo.MarkOutboxDead(requestID, attempts, reason); err != nil {
		log.Printf("[订单] 放弃补偿时标记便签失败: request_id=%s err=%v", requestID, err)
	}
	log.Printf("[订单] 补偿放弃，订单已作废: order=%d request_id=%s 共尝试 %d 次，最后错误: %s",
		orderID, requestID, attempts, reason)
}
