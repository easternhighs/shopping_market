package order

import "time"

// 订单状态。
//
// 为什么用常量、而不是到处写字符串：状态是跨模块的约定
// （handler 判断、后台重试任务判断、前端页面展示都要认它）。
// 写错一个字母编译器不会报错，但订单会卡在一个「谁都不认识」的状态里。
const (
	// StatusStockPending 表示订单已创建，但库存还没扣成功（阶段 16 新增）。
	// 出现它的唯一原因是「订单服务调库存服务失败」（对方挂了、超时、网络不通）。
	// 它是【中间态】，不是最终态：后台重试任务会接着处理它，
	// 要么把订单推进到 StatusPending，要么作废成 StatusCancelled。
	StatusStockPending = "pending_stock"

	// StatusPending 表示库存已扣好、等用户付款。
	// 名字沿用历史的 "pending"（没有改成文档里写的 "pending_pay"）：
	// 它已经被 /orders/:id/pay、/orders/:id/cancel 和前端页面认着，
	// 改名只是更好看，代价却是同时改四个地方 + 迁移历史数据，不划算。
	StatusPending = "pending"

	// StatusPaid 表示已支付。
	StatusPaid = "paid"

	// StatusCancelled 表示已取消（用户主动取消，或者库存不足/重试超限被系统关掉）。
	StatusCancelled = "cancelled"
)

// 本地消息表（order_outbox）里一条消息的四种状态（阶段 16 新增）。
//
// 大白话：order_outbox 就是订单服务抽屉里的一叠「待办便签」。
// 下单时在同一个数据库事务里同时写订单和便签，事务提交之后才照着便签去扣库存；
// 扣成功就把便签划掉，扣不上就留着——后台每隔几秒照便签再试一次。
const (
	// OutboxPending 待处理：还没确认库存扣成功。
	OutboxPending = "pending"
	// OutboxDone 已完成：库存已经扣成功，这张便签没用了。
	OutboxDone = "done"
	// OutboxCancelled 已作废：订单被取消了，不用再去扣库存。
	OutboxCancelled = "cancelled"
	// OutboxDead 已判死：重试次数用光，不再重试（订单同时被关掉）。
	OutboxDead = "dead"
)

// Order 对应数据库里的 orders 表，表示一个订单。
type Order struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RequestID string    `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	UserID    uint      `gorm:"column:user_id;index;not null" json:"user_id"`
	Status    string    `gorm:"type:varchar(32);not null;default:'pending'" json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 对应数据库里的 order_items 表，表示订单里的一个商品明细。
type Item struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	OrderID   uint      `gorm:"column:order_id;index;not null" json:"order_id"`
	SKUID     uint      `gorm:"column:sku_id;not null" json:"sku_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 Item 对应的表名为 order_items。
func (Item) TableName() string {
	return "order_items"
}

// OrderOutbox 对应数据库里的 order_outbox 表，是订单模块的【本地消息表】（阶段 16 新增）。
//
// 它解决的问题，一句话就能说清：**「订单写进库」和「库存扣掉」这两件事，再也没法放进同一个事务里了。**
//
// 阶段 16 之前，库存和订单在同一个 MySQL 库里，扣库存 + 建订单能塞进一个事务：
// 要么全成、要么全不成，简单又可靠。把库存搬进独立的 product 服务之后，
// 跨进程的两个动作没法再「一起成功或一起失败」——可能出现
// 「订单写进去了，扣库存的请求却没发出去」（进程崩了、网线断了），
// 也可能出现「库存扣了，订单没写成」。
//
// 本地消息表是业内最常见、也最好解释的一种解法（核心思路叫「最终一致」）：
//
//  1. 在同一个本地事务里，把订单、订单明细、以及一张「待扣库存」的便签一起写进数据库；
//  2. 事务提交之后才去调库存服务（这一步失败也不要紧，便签在库里躺着）；
//  3. 扣成功 → 便签标记 done，订单进入「待付款」；
//  4. 扣失败（对方暂时不可用）→ 便签留着、订单停在「库存待确认」，
//     后台任务按退避时间（1 秒、2 秒、4 秒……）反复重试；
//  5. 重试次数用光 → 关单 + 便签判死，并且记下最后失败的原因。
//
// 两个「为什么不会出事」：
//   - 为什么不会静默丢单：便签存在数据库里，进程重启、崩溃都不会丢；
//   - 为什么不会重复扣库存：每次重试带的都是同一个 request_id，
//     库存服务那边有一张幂等账本（stock_deducts），同一个 request_id 只认第一次。
//
// Payload 存的是「下单那一刻要扣哪些明细」（JSON 字符串），而不是重试时现去查 order_items——
// 因为重试的时候订单可能已经被改过，照着当时记下的意图去做才说得清账。
type OrderOutbox struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	RequestID string `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	UserID    uint   `gorm:"column:user_id;index;not null" json:"user_id"`
	Payload   string `gorm:"type:text;not null" json:"payload"`
	Status    string `gorm:"type:varchar(32);not null;default:'pending';index" json:"status"`
	Attempts  int    `gorm:"not null;default:0" json:"attempts"`
	// NextRetryAt 是「下一次可以尝试的时间」。
	// 它同时被当成【租约】用：后台任务领走一条便签时，会先把它推后 30 秒，
	// 这样另一个进程在同一时间就不会领到同一条（详见仓储里的 ClaimOutboxRetry）。
	NextRetryAt time.Time `gorm:"column:next_retry_at;index;not null" json:"next_retry_at"`
	LastError   string    `gorm:"type:varchar(512);column:last_error" json:"last_error"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName 指定 OrderOutbox 对应的表名为 order_outbox。
func (OrderOutbox) TableName() string {
	return "order_outbox"
}

// RetryableOrder 把「一条待补偿的订单」需要的东西打成一包：
// 订单本身、订单明细、以及它那张待办便签。
type RetryableOrder struct {
	Order  Order
	Items  []Item
	Outbox OrderOutbox
}
