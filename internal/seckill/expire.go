package seckill

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// 下面是「超时关单」用到的一组常量。
const (
	// DefaultPayTimeout 是默认的付款时限：从订单落库算起，超过这么久还没付钱就关单。
	// 演示时可以用环境变量 SECKILL_PAY_TIMEOUT 调短（比如 20s），不用改代码。
	DefaultPayTimeout = 15 * time.Minute

	// expireScanInterval 是后台扫描的间隔。
	expireScanInterval = 5 * time.Second

	// expireBatchSize 是一次最多处理多少笔超时订单。
	// 为什么要限量：万一积压了几万笔，一次全捞出来会把 Redis 和 MySQL 同时打满；
	// 分批处理，每几秒一轮，几千笔订单一两分钟也就消化完了。
	expireBatchSize = 200

	// expireSweepLookback 是兜底扫描愿意往回看多久。
	//
	// 这个上限是必须的，压测时就真踩过：当时 seckill_orders 里躺着 440 万条
	// 压测遗留的待付款订单，兜底扫描一上线就把它们全部当成「超时」，
	// 每 5 秒啃 200 条、要跑几十个小时，日志也被刷爆。
	// 兜底扫描的职责是「延时队列漏掉的那几笔」，不是「修复停机几个月的历史数据」，
	// 后者属于运维脚本的活。所以这里只扫最近 6 小时，再老的订单一律不动。
	expireSweepLookback = 6 * time.Hour

	// rollbackMarkerTTLSeconds 是「这一单的库存已经还过了」这个标记的存活时间。
	// 它只用来防重复回补，活过最长的订单生命周期就够了，7 天足够宽裕。
	rollbackMarkerTTLSeconds = 7 * 24 * 3600
)

// ErrItemGone 表示要回补库存的秒杀商品行已经不存在了。
// 这种情况下订单照常关掉，只是没有库存可以还——不能因为一行消失的商品数据，
// 让一笔订单永远卡在待付款状态、每几秒被重试一次。
var ErrItemGone = errors.New("秒杀商品行已不存在")

// expireQueueKey 是 Redis 延时队列的 key，类型是有序集合（ZSET）：
//
//	member = request_id（订单的幂等键，天然不重复）
//	score  = 到期时间戳（Unix 秒）
//
// 为什么用 ZSET 做延时队列，而不是给每笔订单起一个定时器：
// 定时器活在进程内存里，进程一重启就全没了；几万笔订单也不可能开几万个定时器。
// ZSET 把「到期时间」直接存成排序用的 score，取「score 小于现在」的成员
// 就是「所有已经到点该关的单」，一次命令捞一批，天然有序、天然去重。
const expireQueueKey = "seckill:orders:expire"

// restoredMarkerKey 是「这笔订单的库存已经回补过」的标记 key。
// 用 request_id 而不是订单主键，是因为延时队列里存的就是 request_id，
// 处理时不必先知道订单号就能判断。
func restoredMarkerKey(requestID string) string {
	return "seckill:order:" + requestID + ":restored"
}

// expireResult 是一笔超时订单的处理结果，用来做每轮汇总统计。
//
// 为什么要单独返回这两个布尔值、而不是在处理过程中直接打日志：
// 积压的时候一轮就是 200 笔，每笔打一行会把日志刷爆。
// 调用方拿到结果后每轮只打一行汇总，日志量从「订单数」降到「轮数」。
type expireResult struct {
	cancelled bool // 这一轮真的把它从待付款改成了已取消
	restored  bool // 这一轮真的把库存还回了商品池
}

// restoreOnceLuaScript 是「超时关单」专用的库存回补脚本。
//
// 和 cache.go 里的 rollbackLuaScript 相比只多了一件事：先写幂等标记。
// 为什么非要这个标记——回补不是「加一下」这么简单，重复执行会真的把库存变大：
//
//	用户限购 3 件，第一单抢了 2 件，超时被关掉 → 库存 +2、该用户已买数回到 0；
//	用户马上又抢了 3 件 → 该用户已买数变成 3；
//	此时如果第一单的回补因为重试又执行一次，脚本只看到「已买数 3 >= 要还的 2」，
//	就会把库存再加 2 —— 凭空多卖 2 件。
//
// 所以必须用 SETNX 抢占标记，只有第一个执行到的调用者才真的回补。
//
//	KEYS[1] 剩余库存
//	KEYS[2] 该用户在该商品上的已购买数量
//	KEYS[3] 回补标记
//	ARGV[1] 本次要回补的数量
//	ARGV[2] 标记的过期秒数
//
// 返回：1 回补成功；-1 用户已购买数量不足（说明早就还过了）；-2 标记已存在，重复调用。
const restoreOnceLuaScript = `
if redis.call("SETNX", KEYS[3], "1") == 0 then
	return -2
end
redis.call("EXPIRE", KEYS[3], tonumber(ARGV[2]))

local bought = tonumber(redis.call("GET", KEYS[2]) or "0")
local quantity = tonumber(ARGV[1])

if bought < quantity then
	return -1
end

redis.call("INCRBY", KEYS[1], quantity)
redis.call("DECRBY", KEYS[2], quantity)
return 1
`

// ScheduleOrdersExpiry 把一批刚成功落库的订单放进延时队列。
//
// 这一批订单的到期时间是一样的（同一个批次的订单几乎同时落库），
// 所以用一次 ZADD 写多个成员，整批只花一次网络往返。
func (c *Cache) ScheduleOrdersExpiry(ctx context.Context, requestIDs []string, deadline time.Time) error {
	if len(requestIDs) == 0 {
		return nil
	}

	members := make([]redis.Z, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		members = append(members, redis.Z{
			Score:  float64(deadline.Unix()),
			Member: requestID,
		})
	}

	return c.client.ZAdd(ctx, expireQueueKey, members...).Err()
}

// DueExpiredOrders 取出已经到点、最多 limit 个订单的 request_id。
// 到点的先取，队列天然按到期时间排好序。
func (c *Cache) DueExpiredOrders(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return c.client.ZRangeByScore(ctx, expireQueueKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(now.Unix(), 10),
		Count: int64(limit),
	}).Result()
}

// RemoveExpiredOrder 把一笔已经处理完的订单从延时队列里摘掉。
// 只有「库存确实处理完了」才调用它，所以中途失败的单会留在队列里等下一轮重试。
func (c *Cache) RemoveExpiredOrder(ctx context.Context, requestID string) error {
	return c.client.ZRem(ctx, expireQueueKey, requestID).Err()
}

// RestoreDeductOnce 把一笔超时关单占用的库存还回去，并且保证只真的还一次。
//
// 返回的 restored 表示「这次调用是不是真的执行了回补」：
// false 说明之前已经还过了（或者标记被别人抢到），调用方不必在意，按成功处理。
//
// 「已购买数不足」这种跳过是正常的（说明别的路径已经处理过这一单），
// 这里刻意不逐条打日志——重试积压时会把日志刷爆，由调用方汇总行数即可。
func (c *Cache) RestoreDeductOnce(ctx context.Context, requestID string, itemID, userID uint, quantity int) (bool, error) {
	stockKey := fmt.Sprintf("seckill:item:%d:stock", itemID)
	userKey := fmt.Sprintf("seckill:item:%d:user:%d", itemID, userID)

	result, err := c.client.Eval(
		ctx,
		restoreOnceLuaScript,
		[]string{stockKey, userKey, restoredMarkerKey(requestID)},
		quantity, rollbackMarkerTTLSeconds,
	).Int()
	if err != nil {
		return false, err
	}

	switch result {
	case 1:
		return true, nil
	case -1, -2:
		return false, nil
	default:
		return false, errors.New("秒杀超时回补出现未知错误")
	}
}

// SetPayTimeout 设置付款时限（从订单落库那一刻开始算）。
// 传 0 或负数表示不改动，保留当前值。
func (s *Service) SetPayTimeout(d time.Duration) {
	if d > 0 {
		s.payTimeout = d
	}
}

// PayTimeout 返回当前的付款时限，演示页面用它显示倒计时。
func (s *Service) PayTimeout() time.Duration {
	return s.payTimeout
}

// scheduleOrderExpiry 把一批刚落库成功的订单放进延时队列，到点后由 ExpireLoop 关单。
//
// 这里是「超时计时」的起点：订单写进 MySQL 的那一刻，就顺手把到期时间写进 Redis。
// 写队列失败不影响成交（订单已经在库里了）——兜底扫描会把漏掉的订单捞回来，
// 所以这里只记日志，不往上抛错。
func (s *Service) scheduleOrderExpiry(ctx context.Context, msgs []OrderMessage) {
	if len(msgs) == 0 || s.payTimeout <= 0 {
		return
	}

	requestIDs := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		requestIDs = append(requestIDs, msg.RequestID)
	}

	deadline := time.Now().Add(s.payTimeout)
	if err := s.cache.ScheduleOrdersExpiry(ctx, requestIDs, deadline); err != nil {
		log.Printf("秒杀订单写入延时队列失败（交给兜底扫描处理）本批 %d 条: %v", len(requestIDs), err)
	}
}

// ExpireLoop 是「超时关单」的后台任务：每隔一小段时间，把该关的订单关掉、把库存还回去。
//
// 它有两条路子，一条快、一条稳：
//
//  1. Redis 延时队列：正常路径。订单落库时就写好了到期时间，到点即处理；
//  2. MySQL 兜底扫描：捞那些「状态还是待付款、但早就过了时限」的订单。
//     Redis 重启、或者订单落库后进程崩溃来不及写队列，都会让第 1 条漏掉订单，
//     而 MySQL 是权威账本，扫一遍一定不会漏（扫描范围有上限，见 expireSweepLookback）。
//
// 两条路子最后都调用同一个 cancelExpiredOrder，靠数据库的条件更新保证只关一次。
func (s *Service) ExpireLoop(ctx context.Context) {
	interval := expireScanInterval
	if s.payTimeout > 0 && s.payTimeout < interval {
		// 演示时会把付款时限调得很短（比如 20 秒），扫描间隔也要跟着变快，
		// 否则「已经超时」和「真的被关掉」之间会差好几秒，看起来像 bug。
		interval = s.payTimeout
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireDueOrders(ctx)
			s.sweepExpiredOrders(ctx)
		}
	}
}

// expireDueOrders 处理延时队列里已经到点的那一批订单。
func (s *Service) expireDueOrders(ctx context.Context) {
	requestIDs, err := s.cache.DueExpiredOrders(ctx, time.Now(), expireBatchSize)
	if err != nil {
		log.Printf("读取秒杀超时队列失败，本轮跳过: %v", err)
		return
	}

	s.runExpireBatch(ctx, "延时队列", requestIDs)
}

// sweepExpiredOrders 兜底扫描 MySQL：把「还是待付款、但已经过了时限」的订单捞出来关掉。
//
// 扫描范围只往回看 expireSweepLookback（见那里的说明），
// 否则停机几天再启动时，这个每 5 秒跑一次的小任务会去啃几百万行历史数据。
func (s *Service) sweepExpiredOrders(ctx context.Context) {
	if s.payTimeout <= 0 {
		return
	}

	now := time.Now()
	orders, err := s.repo.ListExpiredPendingOrders(now.Add(-s.payTimeout), now.Add(-expireSweepLookback), expireBatchSize)
	if err != nil {
		log.Printf("秒杀超时兜底扫描失败，本轮跳过: %v", err)
		return
	}

	requestIDs := make([]string, 0, len(orders))
	for _, order := range orders {
		requestIDs = append(requestIDs, order.RequestID)
	}

	s.runExpireBatch(ctx, "兜底扫描", requestIDs)
}

// runExpireBatch 把一批 request_id 逐个走关单流程，最后只打一行汇总日志。
func (s *Service) runExpireBatch(ctx context.Context, source string, requestIDs []string) {
	if len(requestIDs) == 0 {
		return
	}

	var cancelled, restored, failed int
	for _, requestID := range requestIDs {
		result, err := s.cancelExpiredOrder(ctx, requestID)
		if err != nil {
			// 处理失败就把这条留在队列里，下一轮重试。
			// 千万不能「先清空队列再处理」，那样失败的那条就永远丢了。
			failed++
			log.Printf("秒杀超时关单失败（%s）request_id=%s: %v", source, requestID, err)
			continue
		}
		if result.cancelled {
			cancelled++
		}
		if result.restored {
			restored++
		}
	}

	log.Printf("秒杀超时处理（%s）本轮 %d 笔：关单 %d 笔、回补库存 %d 笔、失败 %d 笔",
		source, len(requestIDs), cancelled, restored, failed)
}

// cancelExpiredOrder 关掉一笔超时订单，并把库存还回商品池。这个方法被重复调用是安全的。
//
// 顺序很重要：**先改 MySQL，再改 Redis**。
// 反过来（先加 Redis 库存）如果 MySQL 那边关单失败，库存就凭空多出来一份，那是超卖；
// 而先改 MySQL 的话，中途失败只是「Redis 暂时少了一份库存」，属于少卖，
// 下一轮重试或者对账任务都能补上，安全得多。
//
// 每一步都做了幂等，所以重复调用不会算错账：
//   - 关单：数据库条件更新（只有还是 pending 才改），同一笔订单只有一个调用者能成功；
//   - 还库存：Redis 脚本先写「已回补」标记，只有第一个调用者会真的加回去。
func (s *Service) cancelExpiredOrder(ctx context.Context, requestID string) (expireResult, error) {
	var result expireResult

	order, err := s.repo.FindOrderByRequestID(requestID)
	if err != nil {
		return result, err
	}
	if order == nil {
		// 队列里有、数据库里没有：只可能是脏数据，直接摘掉。
		return result, s.cache.RemoveExpiredOrder(ctx, requestID)
	}

	needsRestore := false

	switch order.Status {
	case OrderStatusPending:
		cancelled, err := s.cancelOrderInDB(ctx, order)
		if err != nil {
			return result, err
		}
		if cancelled {
			result.cancelled = true
			needsRestore = true
			break
		}

		// 没抢到关单权：可能是别人抢先关了（这份库存该还），
		// 也可能是这一单刚刚被付掉（这份库存不能还）。
		// 必须重新读一次数据库再决定，不能拿手里的旧状态做判断。
		fresh, err := s.repo.FindOrderByRequestID(requestID)
		if err != nil {
			return result, err
		}
		needsRestore = fresh != nil && fresh.Status == OrderStatusCancelled

	case OrderStatusCancelled:
		// 之前关过，但可能只完成了 MySQL 那一步就崩了，这里补上 Redis 那一步。
		// 脚本里的幂等标记保证「已经还过」时不会再还一次。
		needsRestore = true

	default:
		// 已付款，或者以后新增的其它状态：这一单不该被关，把它从延时队列里摘掉。
		return result, s.cache.RemoveExpiredOrder(ctx, requestID)
	}

	if !needsRestore {
		return result, s.cache.RemoveExpiredOrder(ctx, requestID)
	}

	// inFlightExpiry 是在告诉对账任务「先别动这个商品的 Redis 库存」：
	// 此刻 MySQL 的 sold 已经减了、Redis 还没加回来，两边正好差这一份，
	// 这时候覆盖 Redis 会把这笔账算错（详见 ReconcileItem 里的说明）。
	s.inFlightExpiry.Add(1)
	restored, err := s.cache.RestoreDeductOnce(ctx, requestID, order.ItemID, order.UserID, order.Quantity)
	s.inFlightExpiry.Add(-1)
	if err != nil {
		return result, err
	}
	result.restored = restored

	return result, s.cache.RemoveExpiredOrder(ctx, requestID)
}

// cancelOrderInDB 在一个事务里把订单改成已取消，并把占用的库存还回去（sold 减回去）。
// 返回值 cancelled 表示「这次调用是不是真的完成了关单」。
//
// 为什么 sold 也要一起减：对账任务是用「总库存 - sold」反推 Redis 剩余库存的
// （见 ReconcileItem）。如果关单只把 Redis 库存加回去、sold 不动，
// 下一次对账会按「这一份还占着」重新算，刚还回去的库存又被吞掉。
//
// 所以 sold 的语义在这里明确一下：它统计的是「还没被取消的订单占用的库存」，
// 包含待付款的，不包含已取消的。
func (s *Service) cancelOrderInDB(ctx context.Context, order *Order) (bool, error) {
	var cancelled bool

	err := s.repo.Transaction(func(tx *gorm.DB) error {
		ok, err := s.repo.CancelOrderWithTx(tx, order.ID)
		if err != nil {
			return err
		}
		if !ok {
			// 别人先关掉了，或者这一单已经付款了：库存不能再还。
			return nil
		}

		if err = s.repo.RestoreStockWithTx(tx, order.ItemID, order.Quantity); err != nil {
			if errors.Is(err, ErrItemGone) {
				// 商品行已经被删掉了，没有库存可还。订单照常关掉，
				// 不能让一行消失的商品数据把这笔订单永远卡在待付款状态。
				log.Printf("秒杀超时关单：商品行已不存在，跳过库存回补 item_id=%d", order.ItemID)
			} else {
				return err
			}
		}

		cancelled = true
		return nil
	})
	if err != nil {
		return false, err
	}

	return cancelled, nil
}
