package seckill

import "time"

// Activity 对应数据库里的 seckill_activities 表，表示一场秒杀活动。
type Activity struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"type:varchar(191);not null" json:"name"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 对应数据库里的 seckill_items 表，表示这场活动里的一个秒杀商品。
type Item struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	ActivityID   uint      `gorm:"column:activity_id;uniqueIndex:idx_activity_sku;not null" json:"activity_id"`
	SKUID        uint      `gorm:"column:sku_id;uniqueIndex:idx_activity_sku;not null" json:"sku_id"`
	Price        float64   `json:"price"`
	TotalStock   int       `gorm:"not null" json:"total_stock"`
	Sold         int       `gorm:"not null;default:0" json:"sold"`
	PerUserLimit int       `gorm:"not null;default:1" json:"per_user_limit"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// 秒杀订单的状态取值。它描述的是「这一单现在处于哪个阶段」：
//
//	queued    抢购接口刚受理，还在 Kafka 里排队，MySQL 里**没有**这条记录；
//	pending   已经落库，等待付款（超时计时就是从落库那一刻开始的）；
//	paid      已付款，库存真正归这个用户；
//	cancelled 超时未付款被关单，占用的库存已经还回商品池。
const (
	OrderStatusQueued    = "queued"
	OrderStatusPending   = "pending"
	OrderStatusPaid      = "paid"
	OrderStatusCancelled = "cancelled"
)

// activeOrderStatuses 是「算作用户已经买到这个商品」的状态集合。
// 关单（cancelled）之后的订单不算，所以用户还能再抢一次。
var activeOrderStatuses = []string{OrderStatusPending, OrderStatusPaid}

// Order 对应数据库里的 seckill_orders 表，表示一次秒杀下单记录。
type Order struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	RequestID  string `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	ActivityID uint   `gorm:"column:activity_id;index;not null" json:"activity_id"`
	// 下面这两行原来是 uniqueIndex:idx_user_item（同一个用户同一个商品只允许一行）。
	// 加入「超时关单」之后这个唯一约束会挡路：用户超时被关单后想重新抢，
	// 插新行会直接撞唯一索引失败。所以改成普通索引，
	// 「有没有买过」交给业务层按状态判断（见 activeOrderStatuses），
	// 唯一性真正的守门员是 Redis 里那个按用户计数的 key（见 cache.go）。
	ItemID   uint `gorm:"column:item_id;index:idx_user_item;not null" json:"item_id"`
	SKUID    uint `gorm:"column:sku_id;not null" json:"sku_id"`
	UserID   uint `gorm:"column:user_id;index:idx_user_item;index;not null" json:"user_id"`
	Quantity int  `gorm:"not null" json:"quantity"`
	// Status 和 CreatedAt 上的复合索引是给「超时兜底扫描」用的：
	// 它的查询条件是 status = 'pending' 加一段 created_at 范围，
	// 有这条排好序的索引就能直接跳过不相关的行，不必把整张表读一遍
	// （详见 migrate.go 里 ensureExpireSweepIndex 的说明）。
	Status    string    `gorm:"type:varchar(32);not null;default:'pending';index:idx_status_created,priority:1" json:"status"`
	CreatedAt time.Time `gorm:"index:idx_status_created,priority:2" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 Activity 对应的表名。
func (Activity) TableName() string {
	return "seckill_activities"
}

// TableName 指定 Item 对应的表名。
func (Item) TableName() string {
	return "seckill_items"
}

// TableName 指定 Order 对应的表名。
func (Order) TableName() string {
	return "seckill_orders"
}
