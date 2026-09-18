package seckill

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
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
)

// Service 是秒杀模块的业务层。
type Service struct {
	repo            Repository
	cache           *Cache
	queue           *Queue
	metricsObserver func(kind string)
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

// CreateActivity 创建一场秒杀活动。
func (s *Service) CreateActivity(name string, startAt, endAt time.Time) (*Activity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("活动名称不能为空")
	}
	if !endAt.After(startAt) {
		return nil, errors.New("结束时间必须晚于开始时间")
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
		return nil, errors.New("秒杀库存必须大于 0")
	}
	if perUserLimit <= 0 {
		return nil, errors.New("每人限购数量必须大于 0")
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

// Seckill 处理一次秒杀请求。
func (s *Service) Seckill(userID, itemID uint, requestID string, quantity int) (*Order, error) {
	defer s.observeSeckillResult("request")

	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("requestID 不能为空")
	}
	if quantity <= 0 {
		return nil, errors.New("抢购数量必须大于 0")
	}

	//2.查询秒杀商品和活动，校验活动时间
	ctx := context.Background()

	// 缓存优先（cache-aside）：
	//   1) 先问 Redis 有没有商品信息；
	//   2) Redis 未命中（redis.Nil）时再回源 MySQL，并把结果写回 Redis，下次就快了；
	//   3) 其他 Redis 错误直接返回，避免拿不确定的数据继续下单。
	// 好处：秒杀高峰时大多数请求不再查 MySQL，数据库压力大幅下降。
	item, err := s.cache.GetItem(ctx, itemID)
	if errors.Is(err, redis.Nil) {
		item, err = s.repo.FindItemByID(itemID)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, ErrItemNotFound
		}
		// 写回缓存失败不影响本次下单，因此这里忽略错误（下同）。
		_ = s.cache.SetItem(ctx, item)
	} else if err != nil {
		return nil, err
	} else if item == nil {
		return nil, ErrItemNotFound
	}

	// 活动信息同理：先查缓存，未命中再回源 MySQL 并写回。
	activity, err := s.cache.GetActivity(ctx, item.ActivityID)
	if errors.Is(err, redis.Nil) {
		activity, err = s.repo.FindActivityByID(item.ActivityID)
		if err != nil {
			return nil, err
		}
		if activity == nil {
			return nil, ErrActivityNotFound
		}
		_ = s.cache.SetActivity(ctx, activity)
	} else if err != nil {
		return nil, err
	} else if activity == nil {
		return nil, ErrActivityNotFound
	}

	//校验活动时间
	now := time.Now()
	if now.Before(activity.StartAt) {
		return nil, errors.New("秒杀活动尚未开始")
	}
	if now.After(activity.EndAt) {
		return nil, errors.New("秒杀活动已结束")
	}

	//3.把“requestID 防重、用户限购、库存预扣”合并到一次 Redis 原子操作。
	// 优化目的：高并发时不再先访问两次 MySQL，减少数据库连接池压力。
	if quantity > item.PerUserLimit {
		return nil, errors.New("超过每人限购数量")
	}

	if err := s.cache.CheckAndPreDeduct(ctx, requestID, itemID, userID, quantity, item.TotalStock, item.PerUserLimit); err != nil {
		return nil, err
	}

	//4.成功后发送 Kafka 消息，由消费端最终创建 MySQL 订单
	message := OrderMessage{
		RequestID:  requestID,
		ActivityID: activity.ID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
	}
	if err := s.queue.Publish(ctx, message); err != nil {
		return nil, err
	}

	//5.客户端这里先返回“排队中”的订单标识
	queuedOrder := &Order{
		RequestID:  requestID,
		ActivityID: activity.ID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
		Status:     "queued",
	}
	return queuedOrder, nil
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
