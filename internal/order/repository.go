package order

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errOutboxNotClaimable 是仓储内部用的信号：这条便签现在不该由我来处理
// （别人刚领走、已经不是待处理状态、或者重试次数已经用光）。
// 它不算错误，调用方看到 (nil, nil) 跳过就行。
var errOutboxNotClaimable = errors.New("这条 outbox 消息当前不可领取")

// Repository 定义订单数据的读写接口。
type Repository interface {
	FindByRequestID(requestID string) (*Order, error)
	FindByID(id uint) (*Order, error)
	FindItemsByOrderID(orderID uint) ([]Item, error)
	Transaction(fn func(tx *gorm.DB) error) error
	Create(tx *gorm.DB, o *Order) error
	CreateItems(tx *gorm.DB, items []Item) error
	UpdateStatus(orderID uint, status string) error

	// 以下都是阶段 16 新增的「本地消息表」相关操作。
	CreateOutbox(tx *gorm.DB, rec *OrderOutbox) error
	FindOutboxByRequestID(requestID string) (*OrderOutbox, error)
	ClaimOutboxRetry(requestID string, lease time.Duration, now time.Time) (*OrderOutbox, error)
	MarkOutboxDone(requestID string) error
	MarkOutboxCancelled(requestID, reason string) error
	MarkOutboxDead(requestID string, attempts int, lastError string) error
	RescheduleOutbox(requestID string, attempts int, nextRetry time.Time, lastError string) error
	ListRetryableOrders(limit, maxAttempts int, now time.Time) ([]RetryableOrder, error)
	DeleteOrderCascade(tx *gorm.DB, orderID uint, requestID string) error
	CancelStockPendingOrder(orderID uint, requestID, reason string) error
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建订单仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// FindByRequestID 按幂等键查询订单。
func (r *repository) FindByRequestID(requestID string) (*Order, error) {
	var o Order
	if err := r.db.Where("request_id = ?", requestID).First(&o).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// FindByID 按 ID 查询订单。
func (r *repository) FindByID(id uint) (*Order, error) {
	var o Order
	if err := r.db.First(&o, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// FindItemsByOrderID 查询订单明细。
func (r *repository) FindItemsByOrderID(orderID uint) ([]Item, error) {
	var items []Item
	if err := r.db.Where("order_id = ?", orderID).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Transaction 开启一个数据库事务。
func (r *repository) Transaction(fn func(tx *gorm.DB) error) error {
	return r.db.Transaction(fn)
}

// Create 在事务里创建订单。
func (r *repository) Create(tx *gorm.DB, o *Order) error {
	return tx.Create(o).Error
}

// CreateItems 在事务里批量创建订单明细。
func (r *repository) CreateItems(tx *gorm.DB, items []Item) error {
	return tx.Create(&items).Error
}

// UpdateStatus 更新订单状态。
func (r *repository) UpdateStatus(orderID uint, status string) error {
	return r.db.Model(&Order{}).Where("id = ?", orderID).Update("status", status).Error
}

// CreateOutbox 在事务里写下一条「待办便签」。
//
// 它必须和订单在【同一个事务】里写，这是整套补偿机制的地基：
// 只要订单写进去了，便签就一定也在；只要便签在，库存迟早会被扣掉。
func (r *repository) CreateOutbox(tx *gorm.DB, rec *OrderOutbox) error {
	return tx.Create(rec).Error
}

// FindOutboxByRequestID 按幂等键查便签。查不到返回 (nil, nil)。
func (r *repository) FindOutboxByRequestID(requestID string) (*OrderOutbox, error) {
	var rec OrderOutbox
	if err := r.db.Where("request_id = ?", requestID).First(&rec).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// ClaimOutboxRetry 「领取」一条待重试的便签，领到返回它，没领到返回 (nil, nil)。
//
// 为什么要有领取/租约这一步：后台补偿任务可能同时有多个在跑
// （比如运维手滑起了两个进程），如果两个任务同时看到同一条便签、
// 同时去扣库存，就会白跑一趟（虽然幂等能兜住，但没必要）。
//
// 做法：先把这行锁住（SELECT ... FOR UPDATE），确认它还是「待处理」、
// 且到了该重试的时间，然后把「下次重试时间」推到 lease 之后当租约再放手。
// 别的任务在租约到期前查不到它，自然也领不走。
//
// 注意这里的事务【不能】把调库存服务的 HTTP 请求包进来：
// 拿着数据库行锁等网络请求，是所有慢查询事故里最经典的一种。
func (r *repository) ClaimOutboxRetry(requestID string, lease time.Duration, now time.Time) (*OrderOutbox, error) {
	var rec OrderOutbox
	claimed := false

	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("request_id = ?", requestID).
			First(&rec).Error; err != nil {
			return err
		}

		// 逐条判断，任何一条不满足就说明「这条现在不归我管」。
		if rec.Status != OutboxPending {
			return errOutboxNotClaimable
		}
		if rec.Attempts >= OutboxMaxAttempts {
			return errOutboxNotClaimable
		}
		if rec.NextRetryAt.After(now) {
			return errOutboxNotClaimable
		}

		next := now.Add(lease)
		if err := tx.Model(&OrderOutbox{}).Where("id = ?", rec.ID).
			Update("next_retry_at", next).Error; err != nil {
			return err
		}
		rec.NextRetryAt = next
		claimed = true
		return nil
	})

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errOutboxNotClaimable) {
			return nil, nil
		}
		return nil, err
	}
	if !claimed {
		return nil, nil
	}
	return &rec, nil
}

// MarkOutboxDone 把便签标记成「已完成」。
func (r *repository) MarkOutboxDone(requestID string) error {
	return r.db.Model(&OrderOutbox{}).
		Where("request_id = ?", requestID).
		Updates(map[string]any{"status": OutboxDone, "last_error": ""}).Error
}

// MarkOutboxCancelled 把便签作废（订单取消了，不用再去扣库存）。
//
// 条件里带 status = pending 是有意的：已经完成的便签不能被这一刀改回「作废」，
// 否则库里会出现「库存已经扣了，便签却写着作废」这种自相矛盾的记录，
// 以后排查时非常容易把人带沟里。
func (r *repository) MarkOutboxCancelled(requestID, reason string) error {
	return r.db.Model(&OrderOutbox{}).
		Where("request_id = ? AND status = ?", requestID, OutboxPending).
		Updates(map[string]any{"status": OutboxCancelled, "last_error": reason}).Error
}

// MarkOutboxDead 把便签判死：重试次数用光了，永久放弃。
func (r *repository) MarkOutboxDead(requestID string, attempts int, lastError string) error {
	return r.db.Model(&OrderOutbox{}).
		Where("request_id = ?", requestID).
		Updates(map[string]any{"status": OutboxDead, "attempts": attempts, "last_error": lastError}).Error
}

// RescheduleOutbox 登记「这次又失败了，隔多久再试」。
//
// 它一边累加失败次数、一边把下次重试时间往后推，
// 顺便把这次的错误原因记下来（排查时最想知道的就是「它到底为什么一直不成功」）。
func (r *repository) RescheduleOutbox(requestID string, attempts int, nextRetry time.Time, lastError string) error {
	return r.db.Model(&OrderOutbox{}).
		Where("request_id = ? AND status = ?", requestID, OutboxPending).
		Updates(map[string]any{
			"attempts":      attempts,
			"next_retry_at": nextRetry,
			"last_error":    lastError,
		}).Error
}

// ListRetryableOrders 找出这一轮该补偿哪些订单。
//
// 先按便签筛选（待处理 + 到点了 + 还没超过重试上限），再回表看订单状态，
// 订单已经不是「库存待确认」的直接跳过（比如用户已经取消了）。
//
// 为什么先筛便签、而不是一条 SQL join 三张表：便签上已经有索引可用、
// 条数也少（大部分便签很快就变成 done），回表查订单和明细最多几十次，
// 换来的是逻辑一眼能看懂——这个项目里可读性比省那几次查询值钱。
func (r *repository) ListRetryableOrders(limit, maxAttempts int, now time.Time) ([]RetryableOrder, error) {
	var records []OrderOutbox
	if err := r.db.
		Where("status = ? AND next_retry_at <= ? AND attempts < ?", OutboxPending, now, maxAttempts).
		Order("next_retry_at, id").
		Limit(limit).
		Find(&records).Error; err != nil {
		return nil, err
	}

	out := make([]RetryableOrder, 0, len(records))
	for _, rec := range records {
		o, err := r.FindByRequestID(rec.RequestID)
		if err != nil {
			return nil, err
		}
		if o == nil || o.Status != StatusStockPending {
			continue
		}

		items, err := r.FindItemsByOrderID(o.ID)
		if err != nil {
			return nil, err
		}

		out = append(out, RetryableOrder{Order: *o, Items: items, Outbox: rec})
	}
	return out, nil
}

// DeleteOrderCascade 把一张「库存不够、必须撤掉」的订单彻底删干净。
//
// 为什么是「真删」而不是「标记成已取消」：这张订单从来没有真正成立过——
// 库存一件都没扣到、用户也没拿到东西、更没付过钱。留一条 cancelled 记录，
// 用户会在「我的订单」里看到一个莫名其妙的已取消单，还会疑惑自己是不是点错了。
//
// 三张表用同一个事务删，任何一步失败就整体回滚，不会留下「订单没了、明细还在」
// 这种半截数据（order_items 和 order_outbox 都是靠 order_id / request_id 关联的孤儿行）。
func (r *repository) DeleteOrderCascade(tx *gorm.DB, orderID uint, requestID string) error {
	if err := tx.Where("order_id = ?", orderID).Delete(&Item{}).Error; err != nil {
		return err
	}
	if err := tx.Where("request_id = ?", requestID).Delete(&OrderOutbox{}).Error; err != nil {
		return err
	}
	return tx.Delete(&Order{}, orderID).Error
}

// CancelStockPendingOrder 取消一张「库存还没确认」的订单。
//
// 关键点在于：**取消订单和作废便签必须一个事务做完**。
// 如果只改订单状态、便签还留着，后台补偿任务下一秒就会去把库存扣掉，
// 结果变成「库存扣了、订单却已经取消」——库存凭空少了，对不出账。
func (r *repository) CancelStockPendingOrder(orderID uint, requestID, reason string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&OrderOutbox{}).
			Where("request_id = ? AND status = ?", requestID, OutboxPending).
			Updates(map[string]any{"status": OutboxCancelled, "last_error": reason}).Error; err != nil {
			return err
		}
		return tx.Model(&Order{}).Where("id = ?", orderID).Update("status", StatusCancelled).Error
	})
}
