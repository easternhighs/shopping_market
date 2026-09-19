// Package demo 提供阶段 6.2 的「演示控制台」。
//
// 它不是一个电商前台，只是一块「让人看得见系统在干什么」的面板，面试时用来现场演示：
//   - GET  /demo       一个静态页面（原生 JS，不需要 npm、不需要打包工具）；
//   - GET  /demo/data  只读接口，把秒杀商品、Redis 剩余库存、Kafka 在途订单拼成 JSON；
//   - POST /demo/seed  一键造数据：活动、商品、SKU、库存、秒杀商品和演示账号一次准备好。
//
// 为什么单独开一个包，而不是塞进 seckill 模块里：
// 造数据要同时用到 user / product / inventory / seckill 四个模块。如果写在 seckill 内部，
// seckill 就得反过来依赖其他业务模块，模块边界就乱了。放在最外层的 demo 包里，
// 由它来当「组装者」，四个模块之间仍然互不认识，符合模块化单体的约定。
package demo

import (
	_ "embed"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/inventory"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

// demoActivityLimit 是演示页最多显示几场秒杀活动（按活动 ID 从大到小取最近的几场）。
// 为什么要限量：反复点「一键准备」会不断建新活动，不限量的话页面会被历史数据淹掉。
const demoActivityLimit = 5

// indexPage 是演示页面的 HTML。用 go:embed 把它打进可执行文件里，
// 好处是部署时只要一个二进制，不用再单独拷一个 web 目录过去。
//
//go:embed web/index.html
var indexPage []byte

// Handler 是演示控制台的 HTTP 层。
//
// 它自己不碰任何数据库表，只是把各个模块已经对外提供的能力拼起来用，
// 所以这里注入的全是各模块的 Service，而不是 *gorm.DB。
type Handler struct {
	users    *user.Service
	products *product.Service
	stocks   *inventory.Service
	seckill  *seckill.Service

	// seedEnabled 控制「一键造数据」接口开不开，用环境变量 DEMO_SEED=0 关掉。
	// 这个接口会往库里写数据，属于演示专用；真上线时把它关掉即可。
	seedEnabled bool
}

// NewHandler 创建演示控制台 HTTP 层。
func NewHandler(users *user.Service, products *product.Service, stocks *inventory.Service, seckillService *seckill.Service) *Handler {
	return &Handler{
		users:       users,
		products:    products,
		stocks:      stocks,
		seckill:     seckillService,
		seedEnabled: true,
	}
}

// SetSeedEnabled 决定「一键造数据」接口是否开放。
func (h *Handler) SetSeedEnabled(enabled bool) {
	h.seedEnabled = enabled
}

// RegisterRoutes 注册演示控制台的三条路由。
//
// 这里特意**不挂登录中间件**：页面本身要先能打开，才谈得上在里面登录；
// 页面上的抢购按钮走的是正正经经的 /seckill/... 接口，那一条仍然需要 JWT。
func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.GET("/demo", h.Page)
	r.GET("/demo/data", h.Data)
	r.POST("/demo/seed", h.Seed)
}

// Page 处理 GET /demo，把上面 embed 进来的 HTML 原样吐给浏览器。
func (h *Handler) Page(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", indexPage)
}

// demoItem 是演示页要显示的单个秒杀商品，字段名和前端 JS 里用的一一对应。
type demoItem struct {
	ID           uint      `json:"id"`
	ActivityID   uint      `json:"activity_id"`
	ActivityName string    `json:"activity_name"`
	SKUID        uint      `json:"sku_id"`
	Price        float64   `json:"price"`
	TotalStock   int       `json:"total_stock"`
	PerUserLimit int       `json:"per_user_limit"`
	Sold         int       `json:"sold"`
	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`

	// RemainingStock 是 Redis 里还剩多少件。用指针是为了让「没有数据」和「0 件」
	// 在 JSON 里能分开表示：没预热时是 null，真的卖光了才是 0。
	RemainingStock *int `json:"remaining_stock"`
	// StockReady 说明 Redis 里到底有没有这个商品的库存记录。
	StockReady bool `json:"stock_ready"`
}

// Data 处理 GET /demo/data，返回演示页要显示的全部数据。
//
// 三个数据来源刻意分开，正好对应秒杀链路的三个位置：
//   - total_stock / sold：MySQL，最终账本；
//   - remaining_stock：Redis，抢购真正的闸门；
//   - pending：Kafka，还在路上的订单。
//
// 三者对不上的时候，就是讲「异步落库 + 对账」的最佳素材。
func (h *Handler) Data(c *gin.Context) {
	ctx := c.Request.Context()

	activities, err := h.seckill.ListActivities()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 活动按 ID 从大到小排，只留最近几场。
	sort.Slice(activities, func(i, j int) bool { return activities[i].ID > activities[j].ID })
	if len(activities) > demoActivityLimit {
		activities = activities[:demoActivityLimit]
	}

	items := make([]demoItem, 0, len(activities))
	for _, activity := range activities {
		activityItems, err := h.seckill.ListItems(activity.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		for _, item := range activityItems {
			remaining, ready, err := h.seckill.RemainingStock(ctx, item.ID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			entry := demoItem{
				ID:           item.ID,
				ActivityID:   item.ActivityID,
				ActivityName: activity.Name,
				SKUID:        item.SKUID,
				Price:        item.Price,
				TotalStock:   item.TotalStock,
				PerUserLimit: item.PerUserLimit,
				Sold:         item.Sold,
				StartAt:      activity.StartAt,
				EndAt:        activity.EndAt,
				StockReady:   ready,
			}
			if ready {
				entry.RemainingStock = &remaining
			}

			items = append(items, entry)
		}
	}

	// 商品也按 ID 从大到小排，页面默认选中最新造出来的那个。
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })

	c.JSON(http.StatusOK, gin.H{
		"items":       items,
		"pending":     h.seckill.Pending(),
		"server_time": time.Now(),
		"hint":        "remaining_stock 读的是 Redis 预扣库存，sold 读的是 MySQL 账本，两者相差的就是还在 Kafka 里排队的订单",
	})
}
