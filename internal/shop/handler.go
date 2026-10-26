// Package shop 提供阶段 7 的「独立前台」。
//
// 它和 /demo 的分工，一句话就能说清：
//   - /demo 是【给面试官看的仪表盘】——讲系统内部此刻在干什么（Redis 扣了多少、Kafka 压了多少）；
//   - /shop 是【给用户用的前台】——讲用户看到了什么（商品、价格、能不能抢、抢到没有）。
//
// 两个页面看到的是同一套后端、同一条抢购链路，只是站的视角不同。
//
// 它同样是一个「组装者」：自己不碰任何数据库表，只把 product 和 seckill
// 两个模块已经对外提供的能力拼起来。阶段 18 起，这里注入的不再是具体的 Service，
// 而是两个只读接口：单体模式由本地适配器实现，独立进程由 HTTP 客户端实现。
// 页面组装逻辑因此完全不用关心数据是「函数调用」还是「网络请求」回来的。
//
// 对外提供五条路由：
//
//	GET /shop          商品列表页（静态 HTML，用 go:embed 打进二进制）
//	GET /shop/seckill  秒杀独立页（静态 HTML）
//	GET /shop/cart     购物车与结算页（静态 HTML）
//	GET /shop/data     只读数据接口：商品 + SKU + 当前秒杀专场 + Redis 剩余库存
//	GET /shop/my       当前登录用户的秒杀订单（需要 JWT）
//
// 购物车页和另外两个页面有个区别：它自己不带任何服务端数据，页面打开后由浏览器
// 直接去调 cart / order 两个模块已经注册好的接口（/cart/items、/orders）。
// 前台在这里只负责「把页面发出去」，不参与业务——这样购物车怎么改都不用动 shop 包。
//
// 为什么秒杀要单独一个页面：真实电商的秒杀页通常独立部署、独立扩容，
// 因为它要扛瞬时的高流量，和「商品详情」这种读多写少的页面根本不是一个量级。
// 本项目用一个独立 URL（/shop/seckill?item=24）把这个划分体现出来——
// 页面独立，但背后走的是同一套后端接口。
package shop

import (
	_ "embed"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
)

// activityLimit 是前台最多回看几场秒杀活动。
// 反复点 /demo 的「一键准备」会不断建新活动，不限量的话页面会被历史数据淹掉。
const activityLimit = 5

// indexPage 是商品列表页，seckillPage 是秒杀独立页，cartPage 是购物车与结算页。
// 三个都用 go:embed 打进可执行文件，部署时只要一个二进制，不用再单独拷 web 目录。
//
//go:embed web/index.html
var indexPage []byte

//go:embed web/seckill.html
var seckillPage []byte

//go:embed web/cart.html
var cartPage []byte

// contextUserID 必须和 user 模块鉴权中间件写进上下文的键一致（都是 "userID"）。
const contextUserID = "userID"

// Handler 是独立前台的 HTTP 层。
type Handler struct {
	products ProductReader
	seckill  SeckillReader
}

// NewHandler 创建前台 HTTP 层。
func NewHandler(products ProductReader, seckillService SeckillReader) *Handler {
	return &Handler{products: products, seckill: seckillService}
}

// skuView 是前台要显示的一个规格。
type skuView struct {
	ID    uint    `json:"id"`
	Spec  string  `json:"spec"`
	Price float64 `json:"price"`
	// SeckillItemID 不为 0 时，表示这个规格正挂在当前秒杀专场上。
	// 前台靠它决定商品卡上要不要显示「秒杀中」和「立即抢购」。
	SeckillItemID uint `json:"seckill_item_id"`
}

// productView 是前台要显示的一个商品，附带它的全部规格。
type productView struct {
	ID          uint      `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	SKUs        []skuView `json:"skus"`
}

// seckillItemView 是前台要显示的一个秒杀商品。
// 字段和 /demo/data 里的几乎一样，因为要表达的是同一件事；
// 额外多带了 product_id / product_name，前台才能从秒杀商品跳回商品详情。
type seckillItemView struct {
	ID           uint      `json:"id"`
	ActivityID   uint      `json:"activity_id"`
	ActivityName string    `json:"activity_name"`
	ProductID    uint      `json:"product_id"`
	ProductName  string    `json:"product_name"`
	SKUID        uint      `json:"sku_id"`
	Price        float64   `json:"price"`
	TotalStock   int       `json:"total_stock"`
	PerUserLimit int       `json:"per_user_limit"`
	Sold         int       `json:"sold"`
	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`

	// RemainingStock 是 Redis 里还剩多少件。用指针是为了让「没有数据」和「0 件」
	// 在 JSON 里能分开：没预热时是 null，真的卖光了才是 0。
	RemainingStock *int `json:"remaining_stock"`
	StockReady     bool `json:"stock_ready"`

	// PayTimeoutSeconds 是「抢到之后必须在多少秒内付款」，由服务端下发。
	// 前台拿它做倒计时，用户才知道自己不是在无限期地占着库存。
	// 为什么不让前台写死一个 15 分钟：演示时后端是用环境变量把时限调短的
	// （见 main.go 的 seckillPayTimeout），写死的话页面会说「还剩 15 分钟」、
	// 而订单其实 30 秒后就被关掉了，看上去就像系统算错了。
	PayTimeoutSeconds int `json:"pay_timeout_seconds"`
}

// activityView 是前台要显示的一场活动。
type activityView struct {
	ID      uint      `json:"id"`
	Name    string    `json:"name"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

// Page 处理 GET /shop，把商品列表页原样吐给浏览器。
func (h *Handler) Page(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", indexPage)
}

// SeckillPage 处理 GET /shop/seckill，吐出秒杀独立页。
// 具体抢哪个商品由页面自己从 query（?item=24）里读，服务端不做任何预处理。
func (h *Handler) SeckillPage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", seckillPage)
}

// CartPage 处理 GET /shop/cart，吐出购物车与结算页。
//
// 为什么这个页面不需要登录也能打开：页面本身只是一张「壳」，登录判断和取数都在
// 浏览器里做（和 /shop 一样）。真正私密的是 /cart/items 那些接口，它们都在
// 鉴权中间件后面——页面可以公开，数据不行，这个分界别搞混。
func (h *Handler) CartPage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", cartPage)
}

// Data 处理 GET /shop/data，返回前台页面需要的全部只读数据。
//
// 为什么把它做成「一个接口返回全部」，而不是每个页面各查各的：
// 前台页面每隔一两秒就要刷新一次库存，接口越碎、往返越多、页面越容易抖。
// 拼成一个接口，一次就能把列表页和秒杀页要的东西全拿到。
//
// 数据来源分三块，正好对应秒杀链路的三个位置：
//   - products / skus：MySQL，商品目录；
//   - seckill_items 里的 sold：MySQL，最终账本；
//   - seckill_items 里的 remaining_stock：Redis，抢购真正的闸门。
func (h *Handler) Data(c *gin.Context) {
	ctx := c.Request.Context()

	// 第 1 步：把商品和规格读出来，顺便建两张对照表：
	// skuID → 商品ID、商品ID → 商品名。
	// 为什么要建表：秒杀商品记录里只有 SKU ID，前台却要显示「这是哪个商品」，
	// 有对照表就不用为了显示一个名字再多查一次数据库。
	products, err := h.products.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 规格用一条 IN 查询全拿回来，再按 product_id 分好组。
	// 这个接口每秒都会被页面拉一次，如果这里写成「for 每个商品查一次规格」，
	// 就是 N+1：商品越多、查询越多，而它们本来可以合并成一次。
	productIDs := make([]uint, 0, len(products))
	for _, p := range products {
		productIDs = append(productIDs, p.ID)
	}

	allSKUs, err := h.products.ListSKUsByProductIDs(ctx, productIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	skusByProduct := make(map[uint][]product.SKU, len(products))
	for _, s := range allSKUs {
		skusByProduct[s.ProductID] = append(skusByProduct[s.ProductID], s)
	}

	productViews := make([]productView, 0, len(products))
	skuToProductID := make(map[uint]uint, len(allSKUs))
	productNames := make(map[uint]string, len(products))

	for _, p := range products {
		skus := skusByProduct[p.ID]

		skusView := make([]skuView, 0, len(skus))
		for _, s := range skus {
			skuToProductID[s.ID] = p.ID
			skusView = append(skusView, skuView{ID: s.ID, Spec: s.Spec, Price: s.Price})
		}

		productNames[p.ID] = p.Name
		productViews = append(productViews, productView{
			ID:          p.ID,
			Name:        p.Name,
			Description: p.Description,
			Price:       p.Price,
			SKUs:        skusView,
		})
	}

	// 第 2 步：挑出「当前该展示的那场秒杀」。
	// 优先选还没结束、且最近建的一场；如果全都结束了，就退回展示最新的一场，
	// 这样页面不会因为活动过期突然变成空白，而是显示「活动已结束」。
	activities, err := h.seckill.ListActivities(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	sort.Slice(activities, func(i, j int) bool { return activities[i].ID > activities[j].ID })
	if len(activities) > activityLimit {
		activities = activities[:activityLimit]
	}

	now := time.Now()
	var current *seckill.Activity
	for i := range activities {
		if activities[i].EndAt.After(now) {
			current = &activities[i]
			break
		}
	}
	if current == nil && len(activities) > 0 {
		current = &activities[0]
	}

	// 第 3 步：把这场的秒杀商品读出来，并补上 Redis 剩余库存和商品信息。
	itemViews := make([]seckillItemView, 0)
	seckillBySKU := make(map[uint]uint)

	if current != nil {
		items, err := h.seckill.ListItems(ctx, current.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 付款时限在一轮页面数据里只需要取一次。以前写在商品循环里，
		// 有几个秒杀商品就要调几次；拆成 HTTP 接口后那会变成多次网络往返。
		payTimeout, err := h.seckill.PayTimeout(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		payTimeoutSeconds := int(payTimeout.Seconds())

		for _, item := range items {
			remaining, ready, err := h.seckill.RemainingStock(ctx, item.ID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			view := seckillItemView{
				ID:           item.ID,
				ActivityID:   item.ActivityID,
				ActivityName: current.Name,
				SKUID:        item.SKUID,
				Price:        item.Price,
				TotalStock:   item.TotalStock,
				PerUserLimit: item.PerUserLimit,
				Sold:         item.Sold,
				StartAt:      current.StartAt,
				EndAt:        current.EndAt,
				StockReady:   ready,

				PayTimeoutSeconds: payTimeoutSeconds,
			}
			if ready {
				view.RemainingStock = &remaining
			}
			if productID, ok := skuToProductID[item.SKUID]; ok {
				view.ProductID = productID
				view.ProductName = productNames[productID]
			}

			seckillBySKU[item.SKUID] = item.ID
			itemViews = append(itemViews, view)
		}
	}

	// 第 4 步：把「这个规格有没有在秒杀」写回商品上，前台才知道商品卡要不要挂按钮。
	for i := range productViews {
		for j := range productViews[i].SKUs {
			if itemID, ok := seckillBySKU[productViews[i].SKUs[j].ID]; ok {
				productViews[i].SKUs[j].SeckillItemID = itemID
			}
		}
	}

	var activity any
	if current != nil {
		activity = activityView{
			ID:      current.ID,
			Name:    current.Name,
			StartAt: current.StartAt,
			EndAt:   current.EndAt,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"products":      productViews,
		"activity":      activity,
		"seckill_items": itemViews,
		"server_time":   now,
		"hint":          "remaining_stock 是 Redis 里抢购真正的闸门，sold 是 MySQL 的最终账本，两者相差的就是还在 Kafka 里排队的订单",
	})
}

// MyOrders 处理 GET /shop/my，返回当前登录用户的秒杀订单，最新的排在前面。
//
// 为什么只返回「原始订单」，不在服务端把商品名也拼好：
// 拼名字要按 item_id 一条条回查商品，订单一多就变成 N+1 次查询；
// 而页面本来就在轮询 /shop/data，商品名它手上已经有了，前端查表拼一下就行。
// 这是「让服务端少干重复活」的典型取舍——能不让数据库做的事就不让它做。
func (h *Handler) MyOrders(c *gin.Context) {
	userID := c.GetUint(contextUserID)

	orders, err := h.seckill.ListUserOrders(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"orders": orders,
		"hint":   "秒杀订单是异步落库的：抢购成功那一刻还没进这里，等 Kafka 消费端写进 MySQL 之后才会出现",
	})
}
