package order

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrOrderNotFound 表示订单不存在。
var ErrOrderNotFound = errors.New("订单不存在")

// ErrInvalidOrderStatus 表示订单当前状态不允许这次操作。
var ErrInvalidOrderStatus = errors.New("订单状态不允许该操作")

// ErrInvalidArgument 表示请求参数不合法（缺字段、数量为负……）。
// 单独拎出来是为了让 handler 能把它翻成 400，而不是笼统的 500：
// 「你请求写错了」和「我服务出错了」是两件事，客户端该做的处理也不一样。
var ErrInvalidArgument = errors.New("请求参数不合法")

// PlaceItem 是下单请求里的一个商品明细。
type PlaceItem struct {
	SKUID    uint `json:"sku_id"`
	Quantity int  `json:"quantity"`
}

// Service 是订单模块的业务层。
//
// 和阶段 16 之前比，字段里少了一个 inventory.Repository、多了一个 DeductClient：
// 库存已经搬到独立进程里去了，订单这边再也拿不到「同一个事务」，
// 只能从「直接开事务扣」变成「发请求让对面扣」（详见 model.go 里 OrderOutbox 的说明）。
type Service struct {
	repo   Repository
	deduct DeductClient
}

// NewService 创建订单业务层。
func NewService(repo Repository, deduct DeductClient) *Service {
	return &Service{repo: repo, deduct: deduct}
}

// Place 创建订单。
//
// 整个流程按四步走，顺序不能乱：
//
//  1. 校验参数 + 幂等检查（同一个 request_id 直接返回老订单）；
//  2. 本地事务：写订单（状态 pending_stock）+ 写明细 + 写一条 outbox 便签；
//  3. 事务提交之后，跨进程去调库存服务扣库存；
//  4. 按第 3 步的结果决定订单去哪：扣成功变 pending；
//     库存真不够 → 把订单整条撤掉并返回错误；
//     说不清原因（对方挂了）→ 留在 pending_stock，交给后台补偿任务重试。
//
// 注意第 2 步和第 3 步的先后：**必须先提交事务、再去调库存服务**。
// 反过来（先扣库存再写订单）会要命——扣成功了但订单没写成，
// 库存白扣一件，而且没有任何记录能把它找回来。
func (s *Service) Place(ctx context.Context, userID uint, requestID string, items []PlaceItem) (*Order, error) {
	requestID = strings.TrimSpace(requestID)

	if requestID == "" {
		return nil, fmt.Errorf("%w: request_id 不能为空", ErrInvalidArgument)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	for _, item := range items {
		if item.SKUID == 0 || item.Quantity <= 0 {
			return nil, fmt.Errorf("%w: sku_id 必须大于 0，quantity 必须大于 0", ErrInvalidArgument)
		}
	}

	// 幂等检查：同一个 request_id 重复提交（用户狂点、网络重发）只认第一笔。
	// 注意返回老订单时【不】重新扣库存：第一次该扣的已经扣过了。
	if existing, err := s.repo.FindByRequestID(requestID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	payload, err := marshalOutboxPayload(items)
	if err != nil {
		return nil, err
	}

	// 第 2 步：订单 + 明细 + 便签，一个本地事务里全写掉。
	var created *Order
	if err := s.repo.Transaction(func(tx *gorm.DB) error {
		o := &Order{
			RequestID: requestID,
			UserID:    userID,
			// 先记成「库存待确认」：只有库存真扣成功了才会变成 pending。
			// 这个中间态是阶段 16 才有的，它的存在本身就是「最终一致」的体现。
			Status: StatusStockPending,
		}
		if err := s.repo.Create(tx, o); err != nil {
			return err
		}
		// 依赖数据库自增主键：Create 之后 o.ID 才有值，下面才能把它写进明细。
		created = o

		orderItems := make([]Item, 0, len(items))
		for _, item := range items {
			orderItems = append(orderItems, Item{
				OrderID:  o.ID,
				SKUID:    item.SKUID,
				Quantity: item.Quantity,
			})
		}
		if err := s.repo.CreateItems(tx, orderItems); err != nil {
			return err
		}

		return s.repo.CreateOutbox(tx, &OrderOutbox{
			RequestID:   requestID,
			UserID:      userID,
			Payload:     payload,
			Status:      OutboxPending,
			NextRetryAt: time.Now(),
		})
	}); err != nil {
		return nil, err
	}

	// 第 3、4 步：出了事务才调库存服务。
	return s.settleStock(ctx, created, items)
}

// settleStock 拿着刚提交的订单去扣库存，并按结果决定订单的下一步。
func (s *Service) settleStock(ctx context.Context, o *Order, items []PlaceItem) (*Order, error) {
	err := s.deduct.Deduct(ctx, o.RequestID, toDeductItems(items))

	switch {
	case err == nil:
		// 扣成功了。这里两步的顺序是有讲究的：**先改订单状态、再划掉便签**。
		// 反过来的话，便签一划掉（done）而订单状态没改成，就再也没有人来救它了，
		// 订单会永远卡在 pending_stock。按现在的顺序，任何一步失败都还有重试兜着：
		// 重试时会重新扣一次库存（幂等，不会多扣），再把状态推进到 pending。
		if err := s.repo.UpdateStatus(o.ID, StatusPending); err != nil {
			log.Printf("[订单] 库存已扣成功，但订单状态更新失败，交给后台重试: order=%d request_id=%s err=%v", o.ID, o.RequestID, err)
			s.scheduleRetry(o.RequestID, err)
			return o, nil
		}
		if err := s.repo.MarkOutboxDone(o.RequestID); err != nil {
			// 这一步失败不致命：便签还是 pending，后台会重试一次，
			// 那时的扣减请求走幂等账本、直接回「已扣过」，然后再次划掉便签。
			log.Printf("[订单] 标记 outbox 完成失败（不影响订单，后台会重试）: request_id=%s err=%v", o.RequestID, err)
		}

		o.Status = StatusPending
		return o, nil

	case errors.Is(err, ErrStockInsufficient):
		// 库存服务给了确定答案：真的不够。这时订单不该留着——
		// 它一件库存都没占住，用户也没拿到东西。整条撤掉，并把「库存不足」告诉用户。
		if derr := s.repo.Transaction(func(tx *gorm.DB) error {
			return s.repo.DeleteOrderCascade(tx, o.ID, o.RequestID)
		}); derr != nil {
			// 撤单失败时不要谎报成功：如实记日志，并且仍然把错误返回给用户。
			log.Printf("[订单] 库存不足撤单失败，需要人工介入: order=%d request_id=%s err=%v", o.ID, o.RequestID, derr)
		}
		return nil, ErrStockInsufficient

	default:
		// 说不清发生了什么（对方没起来、超时、网络抖动）。
		// 这时候【绝不能】撤单：库存很可能其实已经扣掉了。
		// 正确的做法是先把订单挂住，让后台补偿任务过一会儿再问一次。
		log.Printf("[订单] 调用库存服务失败，订单先挂在「库存待确认」，等后台补偿: order=%d request_id=%s err=%v", o.ID, o.RequestID, err)
		s.scheduleRetry(o.RequestID, err)
		return o, nil
	}
}

// scheduleRetry 登记一次失败：失败次数 +1，并把下次重试时间按退避算法往后推。
func (s *Service) scheduleRetry(requestID string, cause error) {
	attempts := 1
	if rec, err := s.repo.FindOutboxByRequestID(requestID); err == nil && rec != nil {
		attempts = rec.Attempts + 1
	}

	next := time.Now().Add(nextRetryDelay(attempts))
	if err := s.repo.RescheduleOutbox(requestID, attempts, next, cause.Error()); err != nil {
		log.Printf("[订单] 登记重试失败（订单会留在 pending_stock，需要人工看一眼）: request_id=%s err=%v", requestID, err)
	}
}

// Get 查询订单及明细。
func (s *Service) Get(id uint) (*Order, []Item, error) {
	o, err := s.repo.FindByID(id)
	if err != nil {
		return nil, nil, err
	}
	if o == nil {
		return nil, nil, ErrOrderNotFound
	}

	items, err := s.repo.FindItemsByOrderID(id)
	if err != nil {
		return nil, nil, err
	}
	return o, items, nil
}

// Pay 把订单状态改成 paid。
//
// 只允许「pending」（库存已确认）的订单付款：
// 「pending_stock」（库存还没确认）的订单连库存都不一定占住了，
// 让它付款等于卖给用户一件可能不存在的货；
// 「cancelled」「paid」自然也不该再付一次。
func (s *Service) Pay(orderID uint) error {
	o, err := s.repo.FindByID(orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrOrderNotFound
	}

	if o.Status != StatusPending {
		return ErrInvalidOrderStatus
	}

	return s.repo.UpdateStatus(orderID, StatusPaid)
}

// Cancel 把订单状态改成 cancelled。
//
// 两种状态可以取消，处理方式不同：
//   - pending：库存已经扣好了。本项目【不还库存】（和阶段 3 以来的行为一致）；
//   - pending_stock：库存还没扣成功。取消时必须把 outbox 便签一起作废，
//     否则后台补偿任务下一秒又会去扣，变成「库存扣了、订单取消了」。
func (s *Service) Cancel(orderID uint) error {
	o, err := s.repo.FindByID(orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrOrderNotFound
	}

	switch o.Status {
	case StatusPending:
		return s.repo.UpdateStatus(orderID, StatusCancelled)

	case StatusStockPending:
		return s.repo.CancelStockPendingOrder(orderID, o.RequestID, "订单在库存确认期间被用户取消")

	default:
		return ErrInvalidOrderStatus
	}
}
