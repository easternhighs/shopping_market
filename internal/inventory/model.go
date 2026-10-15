package inventory

import "time"

// Stock 对应数据库里的 stocks 表，表示某个 SKU 的库存数量。
type Stock struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SKUID     uint      `gorm:"column:sku_id;uniqueIndex;not null" json:"sku_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DeductItem 是一次扣减请求里的一个明细（阶段 16 新增）。
//
// 它和订单模块的 order.PlaceItem 长得一模一样，但两边各留一份、刻意不共用：
// 这是【两个服务之间的接口约定】，不是同一个进程里的内部结构体。
// 真拆成两个仓库之后，这种结构只能靠一份接口文档对齐，不可能 import 别人的 struct。
type DeductItem struct {
	SKUID    uint `json:"sku_id" binding:"required"`
	Quantity int  `json:"quantity"`
}

// 扣减记录的两个状态。
const (
	// DeductApplied 表示这个 request_id 的库存已经扣成功。
	// 以后再收到同一个 request_id，直接回「成功」，不再扣第二次。
	DeductApplied = "applied"
	// DeductRejected 表示这个 request_id 被拒了（库存不足）。
	// 把它记下来是为了「同一个请求，答案前后一致」：
	// 不能第一次回答「不够」、第二次又扣成功了 —— 那订单那边就对不上账了。
	DeductRejected = "rejected"
)

// DeductRecord 对应数据库里的 stock_deducts 表，是库存服务这一侧的【幂等账本】。
//
// 为什么要这张表：订单服务扣库存失败会重试，网络也可能把同一个请求送两次。
// 库存服务必须能回答一个问题：「这个 request_id 我是不是已经处理过了？」
// request_id 上的唯一索引就是答案的载体 —— 数据库只允许同一个键存在一行，
// 重复的插不进去，于是「重复请求」自然被挡住了。
type DeductRecord struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RequestID string    `gorm:"type:varchar(191);column:request_id;uniqueIndex;not null" json:"request_id"`
	Status    string    `gorm:"type:varchar(32);not null" json:"status"`
	LastError string    `gorm:"type:varchar(512);column:last_error" json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 指定 DeductRecord 对应的表名为 stock_deducts。
func (DeductRecord) TableName() string {
	return "stock_deducts"
}
