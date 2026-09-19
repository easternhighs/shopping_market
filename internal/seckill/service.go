package seckill

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	// ErrActivityNotFound 表示秒杀活动不存在。
	ErrActivityNotFound = errors.New("秒杀活动不存在")
	// ErrItemNotFound 表示秒杀商品不存在。
	ErrItemNotFound = errors.New("秒杀商品不存在")
	// ErrInsufficientStock 表示秒杀库存不足。
	ErrInsufficientStock = errors.New("秒杀库存不足")
	// ErrAlreadySeckilled 表示该用户已经抢购过这个秒杀商品。
	ErrAlreadySeckilled = errors.New("您已经抢购过该商品")
	// ErrDeductUnknown 表示 Redis 扣减结果未知。
	ErrDedectUnknown = errors.New("库存扣减未知错误")

	// 下面这些是[请求被业务拒绝]的错误，它们**不是服务故障**。
	// HTTP 层必须把它们映射成 4xx：500 的含义是[我们这边坏了]，
	// 客户端和监控看到都会当成 bug，去重试、去告警；
	// 而这类错误重试一万次结果也一样，应该明确告诉调用方[这次请求本身不成立]。
	//
	// ErrInvalidRequest 表示请求参数不合法（→ 400）。
	ErrInvalidRequest = errors.New("请求参数不合法")
	// ErrDuplicateRequest 表示同一个 request_id 已经处理过（→ 409）。
	ErrDuplicateRequest = errors.New("重复的 request_id")
	// ErrPerUserLimitExceeded 表示超过每人限购数量（→ 409）。
	ErrPerUserLimitExceeded = errors.New("超出每人购买限制")
	// ErrActivityNotStarted 表示秒杀活动尚未开始（→ 409）。
	ErrActivityNotStarted = errors.New("秒杀活动尚未开始")
	// ErrActivityEnded 表示秒杀活动已结束（→ 409）。
	ErrActivityEnded = errors.New("秒杀活动已结束")
	// ErrRateLimited 表示这个用户在当前时间窗口内请求太频繁（→ 429）。
	ErrRateLimited = errors.New("抢购请求过于频繁")
)

// Service 是秒杀模块的业务层。
type Service struct {
	repo              Repository
	cache             *Cache
	queue             *Queue
	metricsObserver   func(kind string)
	rateLimitObserver func()
}

// NewService 创建秒杀业务层。
func NewService(repo Repository, cache *Cache, queue *Queue) *Service {
	return &Service{repo: repo, cache: cache, queue: queue}
}

// SetMetricsObserver 设置秒杀指标回调，可选。
// 传入 nil 表示不统计秒杀结果。
func (s *Service) SetMetricsObserver(fn func(kind string)) {
	s.metricsObserver = fn
}

// observeSeckillResult 向指标系统报告一次秒杀结果。
// kind 可以是 success、failure、duplicate。
func (s *Service) observeSeckillResult(kind string) {
	if s.metricsObserver != nil {
		s.metricsObserver(kind)
	}
}

// SetRateLimitObserver 设置限流指标回调，可选。
// 抢购接口的限流发生在 Redis 脚本内部，中间件看不到，所以要由业务层主动上报。
func (s *Service) SetRateLimitObserver(fn func()) {
	s.rateLimitObserver = fn
}

// observeRateLimited 向指标系统报告一次被限流的请求。
func (s *Service) observeRateLimited() {
	if s.rateLimitObserver != nil {
		s.rateLimitObserver()
	}
}

// CreateActivity 创建一场秒杀活动。
func (s *Service) CreateActivity(name string, startAt, endAt time.Time) (*Activity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: 活动名称不能为空", ErrInvalidRequest)
	}
	if !endAt.After(startAt) {
		return nil, fmt.Errorf("%w: 结束时间必须晚于开始时间", ErrInvalidRequest)
	}

	a := &Activity{Name: name, StartAt: startAt, EndAt: endAt}
	if err := s.repo.CreateActivity(a); err != nil {
		return nil, err
	}
	return a, nil
}

// ListActivities 查询全部秒杀活动。
func (s *Service) ListActivities() ([]Activity, error) {
	return s.repo.ListActivities()
}

// CreateItem 为某场活动添加一个秒杀商品。
func (s *Service) CreateItem(activityID, skuID uint, price float64, totalStock, perUserLimit int) (*Item, error) {
	activity, err := s.repo.FindActivityByID(activityID)
	if err != nil {
		return nil, err
	}
	if activity == nil {
		return nil, ErrActivityNotFound
	}
	if totalStock <= 0 {
		return nil, fmt.Errorf("%w: 秒杀库存必须大于 0", ErrInvalidRequest)
	}
	if perUserLimit <= 0 {
		return nil, fmt.Errorf("%w: 每人限购数量必须大于 0", ErrInvalidRequest)
	}

	item := &Item{
		ActivityID:   activityID,
		SKUID:        skuID,
		Price:        price,
		TotalStock:   totalStock,
		PerUserLimit: perUserLimit,
	}
	if err := s.repo.CreateItem(item); err != nil {
		return nil, err
	}
	return item, nil
}

// ListItems 查询某场活动下的全部秒杀商品。
func (s *Service) ListItems(activityID uint) ([]Item, error) {
	return s.repo.ListItemsByActivityID(activityID)
}

// RemainingStock 读取某个秒杀商品在 Redis 里的剩余库存。
//
// 第二个返回值表示「Redis 里到底有没有这个商品的库存记录」：
// 没有说明缓存还没热起来（第一次抢购会自动建立），不等于库存为 0。
//
// 和 RemainingStock 类似的只读方法都是给演示控制台、排查问题用的，
// 不参与抢购热路径，所以可以放心走一次独立的 Redis 往返。
func (s *Service) RemainingStock(ctx context.Context, itemID uint) (int, bool, error) {
	return s.cache.Stock(ctx, itemID)
}

// Pending 报告此刻有多少秒杀订单「已经发进 Kafka、但还没落进 MySQL」。
//
// 演示控制台用它显示「Kafka 堆积」：这个数字大于 0，说明 Redis 已经扣了库存、
// 但 MySQL 还没记账，此刻去查 MySQL 的 sold 会偏小。这也正是对账任务
// 必须等队列安静下来才敢动 Redis 库存的原因（见 Queue.Settled）。
func (s *Service) Pending() int64 {
	return s.queue.Pending()
}

// rateLimitPerUser 和 rateLimitWindow 是抢购接口对单个用户的限流阈值。
//
// 为什么限流放在 Redis 脚本里，而不是像其他接口那样放在中间件里：
// 中间件的限流计数是一次独立的 Redis 往返；放进脚本后，它跟扣减共用同一次往返，
// 等于「限流」这件事不再额外花 RTT。
// 代价是限流维度从 IP 变成了用户——抢购接口必须登录，按用户限流反而更准
// （同一个人换 IP 也躲不掉），而 IP 维度的防护仍然由秒杀管理接口的中间件负责。
const (
	rateLimitPerUser = 100
	rateLimitWindow  = time.Second
)

// Seckill 处理一次秒杀请求。
//
// 热路径只有一次 Redis 往返：限流、读缓存、校验活动时间、防重、限购、预扣库存
// 全部在同一个 Lua 脚本里完成（见 cache.go 的 hotPathLuaScript）。
// 只有 Redis 里还没有这个商品的缓存时，才会回源 MySQL 填充缓存后重试一次。
func (s *Service) Seckill(userID, itemID uint, requestID string, quantity int) (*Order, error) {
	defer s.observeSeckillResult("request")

	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("%w: requestID 不能为空", ErrInvalidRequest)
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("%w: 抢购数量必须大于 0", ErrInvalidRequest)
	}

	ctx := context.Background()
	req := FastPathRequest{
		RequestID:  requestID,
		ItemID:     itemID,
		UserID:     userID,
		Quantity:   quantity,
		Now:        time.Now(),
		RateLimit:  rateLimitPerUser,
		RateWindow: rateLimitWindow,
	}

	item, err := s.cache.TrySeckill(ctx, req)
	if errors.Is(err, errCacheNotReady) {
		// 冷路径：Redis 里没有这个商品的信息（第一次抢、缓存过期、或者服务刚重启）。
		// 回源 MySQL 把缓存补齐，然后重试一次；仍然失败就交给下面统一处理。
		if warmErr := s.warmUpCache(ctx, itemID); warmErr != nil {
			return nil, warmErr
		}
		req.Now = time.Now()
		item, err = s.cache.TrySeckill(ctx, req)
	}
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			s.observeRateLimited()
		}
		return nil, err
	}

	// 预扣成功，把这次抢购交给 Kafka 排队，由消费端最终写进 MySQL。
	message := OrderMessage{
		RequestID:  requestID,
		ActivityID: item.ActivityID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
	}
	if err := s.queue.Publish(ctx, message); err != nil {
		return nil, err
	}

	// 客户端这里先返回“排队中”的订单标识
	return &Order{
		RequestID:  requestID,
		ActivityID: item.ActivityID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
		Status:     "queued",
	}, nil
}

// warmUpCache 回源 MySQL 读出秒杀商品和它所属的活动，写进 Redis 供热路径使用。
// 它只在热路径报告「缓存没准备好」时被调用，属于低频的冷路径。
func (s *Service) warmUpCache(ctx context.Context, itemID uint) error {
	item, activity, err := s.loadItemAndActivity(itemID)
	if err != nil {
		return err
	}

	// 写缓存失败不影响本次下单：这次照样往下走，下次再走一遍冷路径即可。
	_ = s.cache.WarmUp(ctx, item, activity)
	return nil
}

// WarmUpStock 把一个秒杀商品的缓存建好，并让 Redis 库存等于「总库存 − 已售」。
//
// 它和 warmUpCache 的分工：
//   - warmUpCache 是「自愈」：抢购时发现缓存没了才回源，只补**信息**缓存，
//     库存不碰（由 Lua 脚本在处理第一次请求时按总库存自动建出来），写失败也无所谓；
//   - WarmUpStock 是「主动预热」：信息缓存和库存一起建好，给演示控制台造完数据后调用，
//     这样页面上立刻就有库存数字，第一次抢购也不用走冷路径。
func (s *Service) WarmUpStock(ctx context.Context, itemID uint) error {
	item, activity, err := s.loadItemAndActivity(itemID)
	if err != nil {
		return err
	}
	if err := s.cache.WarmUp(ctx, item, activity); err != nil {
		return err
	}
	return s.cache.SetStock(ctx, item.ID, item.TotalStock-item.Sold)
}

// loadItemAndActivity 从 MySQL 读出秒杀商品和它所属的活动。
// 两者任意一个不存在，都会返回对应的哨兵错误。
func (s *Service) loadItemAndActivity(itemID uint) (*Item, *Activity, error) {
	item, err := s.repo.FindItemByID(itemID)
	if err != nil {
		return nil, nil, err
	}
	if item == nil {
		return nil, nil, ErrItemNotFound
	}

	activity, err := s.repo.FindActivityByID(item.ActivityID)
	if err != nil {
		return nil, nil, err
	}
	if activity == nil {
		return nil, nil, ErrActivityNotFound
	}

	return item, activity, nil
}

// BatchResult 是一批消息“处理完之后各自的下场”。
//
// 为什么需要它：以前一条消息一个事务，处理结果就是一个 error，
// 成功还是失败一目了然。现在一次处理一批，事务要么整批成功、要么整批回滚，
// 光靠一个 error 已经说不清“这批里哪几条算成功、哪几条要回补 Redis”，
// 所以要有一张清单，把每条消息的归属记下来交给调用方。
type BatchResult struct {
	Success    []OrderMessage // 已经写进 MySQL 的消息
	Duplicates []OrderMessage // MySQL 里已经有对应订单、直接跳过的消息
	Failed     []OrderMessage // 没能写进 MySQL、需要回补 Redis 的消息
}

// processOrdersBatch 把一批 Kafka 消息合并成“一个 MySQL 事务”落库。
//
// 为什么要合并：原来 200 条消息 = 200 个事务，每个事务都要执行
// UPDATE seckill_items SET sold = sold + 1 WHERE id = ?，
// 也就是 200 次去抢同一行的行锁。MySQL 同一行同一时刻只允许一个事务修改，
// 所以 6 个消费者最后全挤在一个收银台前排队——压测实测消费速率只有 80 条/秒，
// 平均每次等锁 65 毫秒，行锁等待次数几乎等于事务提交次数。
// 合并成 1 个事务后，整批只 UPDATE 一次、只批量 INSERT 一次、只抢一次行锁。
//
// 为什么必须放在一个事务里：“扣库存”和“插订单”必须同生共死，
// 要么都成功、要么都别做；否则会出现“库存减了但订单没插进去”的半成品状态，
// Redis 和 MySQL 就再也对不上了。
//
// 返回值怎么理解：正常时 Success + Duplicates 就是这一批的全部消息，Failed 为空；
// 中间任何一步出错就整批回滚，并把这一批全部消息记成 Failed（MySQL 一条都没写进去）。
//
// 实现分 5 步，全部在同一个事务里完成：
//
// 第 1 步（查重）：把所有 msg.RequestID 收集成 []string，
// 调用 FindOrdersByRequestIDsWithTx，一条 IN 查询问完整批。
//
// 第 2 步（限购）：FindOrdersByUserAndItemWithTx 一次只能查一个 itemID，
// 所以先按 ItemID 分组，每组收集 userIDs 各查一次。
//
// 第 3 步（扣库存）：把每组消息的 Quantity 累加，按 itemID 调用一次 DeductStockBatch。
//
// 第 4 步（插订单）：剩下的消息拼成 []Order，CreateOrdersWithTx 一次插完。
//
// 第 5 步（填清单）：插成功的记 Success，前面筛出来的记 Duplicates。
func (s *Service) processOrdersBatch(ctx context.Context, msgs []OrderMessage) *BatchResult {
	if len(msgs) == 0 {
		return &BatchResult{}
	}

	var success []OrderMessage
	var duplicates []OrderMessage

	err := s.repo.Transaction(func(tx *gorm.DB) error {
		// 第 1 步：一条 IN 查询找出整批里已经存在的 request_id。
		requestIDs := make([]string, 0, len(msgs))
		for _, msg := range msgs {
			requestIDs = append(requestIDs, msg.RequestID)
		}

		existingOrders, err := s.repo.FindOrdersByRequestIDsWithTx(tx, requestIDs)
		if err != nil {
			return err
		}

		// seenRequestIDs 里除了 MySQL 里已有的，还要装“本批已经处理过的”。
		// 同一批里出现两条相同 request_id（Kafka 重投落到同一批）时，
		// 它们会撞上 request_id 唯一索引把整批插入打回，所以提前当重复处理。
		seenRequestIDs := make(map[string]struct{}, len(msgs))
		for _, existingOrder := range existingOrders {
			seenRequestIDs[existingOrder.RequestID] = struct{}{}
		}

		pending := make([]OrderMessage, 0, len(msgs))
		for _, msg := range msgs {
			if _, ok := seenRequestIDs[msg.RequestID]; ok {
				duplicates = append(duplicates, msg)
				continue
			}
			seenRequestIDs[msg.RequestID] = struct{}{}
			pending = append(pending, msg)
		}

		// 第 2 步：按 itemID 分组，每组一次查出“已经买过这个商品”的用户。
		itemIDs := make([]uint, 0, len(pending))
		groups := make(map[uint][]OrderMessage)
		for _, msg := range pending {
			if _, ok := groups[msg.ItemID]; !ok {
				itemIDs = append(itemIDs, msg.ItemID)
			}
			groups[msg.ItemID] = append(groups[msg.ItemID], msg)
		}

		valid := make([]OrderMessage, 0, len(pending))
		for _, itemID := range itemIDs {
			group := groups[itemID]

			userIDs := make([]uint, 0, len(group))
			for _, msg := range group {
				userIDs = append(userIDs, msg.UserID)
			}

			existingUserOrders, err := s.repo.FindOrdersByUserAndItemWithTx(tx, itemID, userIDs)
			if err != nil {
				return err
			}

			boughtUsers := make(map[uint]struct{}, len(existingUserOrders))
			for _, existingOrder := range existingUserOrders {
				boughtUsers[existingOrder.UserID] = struct{}{}
			}

			// 这里同样要防“本批里同一个用户两条消息”：它们会撞上
			// (item_id, user_id) 唯一索引，所以第二条也当重复处理。
			for _, msg := range group {
				if _, ok := boughtUsers[msg.UserID]; ok {
					duplicates = append(duplicates, msg)
					continue
				}
				boughtUsers[msg.UserID] = struct{}{}
				valid = append(valid, msg)
			}
		}

		// 第 3 步：把每个 itemID 的数量加起来，整批只 UPDATE 一次。
		totals := make(map[uint]int, len(itemIDs))
		for _, msg := range valid {
			totals[msg.ItemID] += msg.Quantity
		}
		for _, itemID := range itemIDs {
			total := totals[itemID]
			if total == 0 {
				continue
			}
			if err := s.repo.DeductStockBatch(tx, itemID, total); err != nil {
				return err
			}
		}

		// 第 4 步：一次多值 INSERT 把订单全部写进去。
		orders := make([]Order, 0, len(valid))
		for _, msg := range valid {
			orders = append(orders, Order{
				RequestID:  msg.RequestID,
				ActivityID: msg.ActivityID,
				ItemID:     msg.ItemID,
				SKUID:      msg.SKUID,
				UserID:     msg.UserID,
				Quantity:   msg.Quantity,
				Status:     "pending",
			})
		}
		if err := s.repo.CreateOrdersWithTx(tx, orders); err != nil {
			return err
		}

		// 第 5 步：走到这里说明库存和订单都已经在 MySQL 里了。
		success = valid
		return nil
	})
	if err != nil {
		// 事务已经整体回滚，MySQL 一条都没写进去，所以整批都要回补 Redis。
		log.Print("批量落库失败，整批回补 Redis: ", err)
		return &BatchResult{Failed: msgs}
	}

	return &BatchResult{Success: success, Duplicates: duplicates}
}
