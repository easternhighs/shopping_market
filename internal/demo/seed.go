package demo

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/inventory"
	"shopping_market/internal/product"
	"shopping_market/internal/user"
)

// demoPassword 是全部演示账号的统一密码。
// 演示账号由「注册」接口创建（会走正常的密码哈希），所以密码必须是固定的，
// 现场演示时才能直接登录，不用手填注册表单。
const demoPassword = "demo1234"

// demoProduct 描述一个演示商品：商品本身 + 它的一个规格 + 这个规格在秒杀里的配置。
//
// 为什么要一次准备好几个商品，而不是像以前那样只准备一个：
// 前台商城（/shop）有商品列表页，列表上只有一个商品太单薄，
// 也看不出「列表 → 详情 → 秒杀独立页」这条跳转是怎么走的。
// 现在四个商品的库存、价格、限购各不相同，页面上能同时看到「快卖光」和「还剩很多」两种进度条。
//
// 每个商品的字段都写死、不带随机后缀：反复点「一键准备」时靠这些固定值做幂等判断，
// 已经存在的商品和规格直接复用，不会越点越多。
type demoProduct struct {
	Name         string
	Description  string
	Price        float64 // 商品标价（原价）
	Spec         string  // 规格名
	SKUPrice     float64 // 规格价格
	SeckillPrice float64 // 秒杀价
	TotalStock   int     // 这个商品放多少件秒杀库存
	PerUserLimit int     // 每人限购几件
}

// demoProducts 是「一键准备」要保证存在的四个演示商品。
// 库存刻意拉开档次：30 / 80 / 200 / 500，
// 演示时一眼就能看出「旗舰机秒无」和「充电宝随便买」的区别。
var demoProducts = []demoProduct{
	{
		Name:         "演示机皇 · 星海 X1",
		Description:  "上一代旗舰，秒杀价直降两千。库存只有 30 台，用来演示「瞬间抢光」。",
		Price:        4999,
		Spec:         "12GB+256GB",
		SKUPrice:     2999,
		SeckillPrice: 1999,
		TotalStock:   30,
		PerUserLimit: 1,
	},
	{
		Name:         "演示手表 · 星河 Watch",
		Description:  "全天候心率监测，库存 80 只，演示「抢一会儿才抢完」。",
		Price:        1299,
		Spec:         "46mm 黑色",
		SKUPrice:     999,
		SeckillPrice: 599,
		TotalStock:   80,
		PerUserLimit: 1,
	},
	{
		Name:         "演示耳机 · 声浪 Air",
		Description:  "半入耳降噪耳机，库存 200 副，每人可以买 2 副——用来演示「限购」这条规则。",
		Price:        799,
		Spec:         "白色",
		SKUPrice:     399,
		SeckillPrice: 199,
		TotalStock:   200,
		PerUserLimit: 2,
	},
	{
		Name:         "演示充电宝 · 随行 20000",
		Description:  "20000mAh 双向快充，库存 500 个。压测时用它，不容易几秒钟就被打空。",
		Price:        249,
		Spec:         "20000mAh",
		SKUPrice:     149,
		SeckillPrice: 99,
		TotalStock:   500,
		PerUserLimit: 3,
	},
}

// seedRequest 是「一键准备」接口的请求体，字段都可以不填，不填就用默认值。
type seedRequest struct {
	// UserCount 要准备几个演示账号（demo1、demo2……）。
	UserCount int `json:"user_count"`
	// TotalStock 传了就把所有秒杀商品的库存统一设成这个数；
	// 不传（或者传 0）则每个商品用 demoProducts 里各自预设的库存。
	TotalStock int `json:"total_stock"`
}

// seededItem 是「一键准备」返回给调用方的一个秒杀商品。
type seededItem struct {
	ProductID    uint    `json:"product_id"`
	ProductName  string  `json:"product_name"`
	SKUID        uint    `json:"sku_id"`
	ItemID       uint    `json:"item_id"`
	Price        float64 `json:"price"`
	TotalStock   int     `json:"total_stock"`
	PerUserLimit int     `json:"per_user_limit"`
}

// Seed 处理 POST /demo/seed：把演示要用的数据一次性准备好。
//
// 整个过程分 6 步，每一步都先说「为什么需要它」：
//
//	第 1 步 商品：秒杀商品要挂在一个真实存在的商品下面，不然数据是悬空的；
//	第 2 步 SKU：用户真正购买的是 SKU（具体规格），秒杀商品记录里存的是 SKU ID；
//	第 3 步 普通库存：秒杀本身不用它，但少了它数据看起来不完整，顺手补上；
//	第 4 步 秒杀活动：定一个「5 分钟前开始、2 小时后结束」的时间窗，方便随到随抢；
//	第 5 步 秒杀商品：把每个 SKU 挂进同一场活动，各自指定库存和每人限购；
//	第 6 步 预热 Redis：把商品信息、活动时间窗和剩余库存写进 Redis，
//	        页面一打开就能看到库存进度条，第一次抢购也不用走冷路径。
//	最后 演示账号：创建 demo1、demo2…… 并用统一的密码。
//
// 幂等性：商品、SKU、库存、账号都是「有就复用，没有才建」；
// 只有活动和秒杀商品每次都是新建的，因为每次演示都想要一场「库存满、时间还没过」的新活动。
func (h *Handler) Seed(c *gin.Context) {
	if !h.seedEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "演示造数据接口已关闭（服务启动时设置了 DEMO_SEED=0）"})
		return
	}

	var req seedRequest
	// 允许不带请求体直接调用，所以只在有请求体时才解析。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
			return
		}
	}

	if req.UserCount <= 0 {
		req.UserCount = 3
	}
	if req.UserCount > 50 {
		req.UserCount = 50
	}

	ctx := c.Request.Context()

	// 第 4 步先做：这一场活动要把下面所有的秒杀商品都装进去。
	// 开始时间往前挪 5 分钟，避免「刚建好却提示还没开始」。
	now := time.Now()
	activity, err := h.seckill.CreateActivity(
		"演示秒杀专场 "+now.Format("01-02 15:04"),
		now.Add(-5*time.Minute),
		now.Add(2*time.Hour),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建秒杀活动失败: " + err.Error()})
		return
	}

	// 逐个商品走完第 1 步到第 6 步。
	// 任何一个商品失败都直接返回：留一半数据在现场更难解释。
	seeded := make([]seededItem, 0, len(demoProducts))
	for _, dp := range demoProducts {
		prod, err := h.ensureProduct(dp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建商品失败: " + err.Error()})
			return
		}

		sku, err := h.ensureSKU(prod.ID, dp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建 SKU 失败: " + err.Error()})
			return
		}

		if err := h.ensureStock(sku.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建库存失败: " + err.Error()})
			return
		}

		stock := dp.TotalStock
		if req.TotalStock > 0 {
			stock = req.TotalStock
		}

		item, err := h.seckill.CreateItem(activity.ID, sku.ID, dp.SeckillPrice, stock, dp.PerUserLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建秒杀商品失败: " + err.Error()})
			return
		}

		// 第 6 步：预热 Redis。不预热的话前台页面的进度条会是「未预热」，
		// 第一次抢购也要多走一次冷路径回源 MySQL。
		if err := h.seckill.WarmUpStock(ctx, item.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "预热 Redis 失败: " + err.Error()})
			return
		}

		seeded = append(seeded, seededItem{
			ProductID:    prod.ID,
			ProductName:  prod.Name,
			SKUID:        sku.ID,
			ItemID:       item.ID,
			Price:        item.Price,
			TotalStock:   item.TotalStock,
			PerUserLimit: item.PerUserLimit,
		})
	}

	usernames, err := h.ensureUsers(req.UserCount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建演示账号失败: " + err.Error()})
		return
	}

	// items 是完整的清单；下面那几个不带 items 的字段是「第一个商品」的信息，
	// 保留它们是为了让老版本演示控制台的 JS 继续能用，不用跟着一起改。
	first := seeded[0]

	c.JSON(http.StatusOK, gin.H{
		"activity_id":   activity.ID,
		"activity_name": activity.Name,
		"start_at":      activity.StartAt,
		"end_at":        activity.EndAt,
		"items":         seeded,
		"users":         usernames,
		"password":      demoPassword,

		"product_id":     first.ProductID,
		"sku_id":         first.SKUID,
		"item_id":        first.ItemID,
		"total_stock":    first.TotalStock,
		"per_user_limit": first.PerUserLimit,
		"price":          first.Price,

		"hint": fmt.Sprintf("前台去 /shop 看列表，秒杀去 /shop/seckill?item=%d；"+
			"用 %s 里的任意账号 + 密码 %s 登录", first.ItemID, usernames[0], demoPassword),
	})
}

// ensureProduct 找到演示商品，找不到才建。
// 商品模块没有「按名字查」的接口，所以用 List 全量取回来在内存里比一遍——
// 演示数据的数据量很小，这样写最省事，也不用为此再给商品模块加接口。
func (h *Handler) ensureProduct(dp demoProduct) (*product.Product, error) {
	products, err := h.products.List()
	if err != nil {
		return nil, err
	}

	for i := range products {
		if products[i].Name == dp.Name {
			return &products[i], nil
		}
	}

	return h.products.Create(dp.Name, dp.Description, dp.Price)
}

// ensureSKU 找到「这个商品 + 这个规格」的 SKU，找不到才建。
func (h *Handler) ensureSKU(productID uint, dp demoProduct) (*product.SKU, error) {
	sku, err := h.products.FindSKUByProductAndSpec(productID, dp.Spec)
	if err != nil {
		return nil, err
	}
	if sku != nil {
		return sku, nil
	}

	return h.products.CreateSKU(productID, dp.Spec, dp.SKUPrice)
}

// ensureStock 保证 SKU 有一条库存记录。
// 库存模块的 Get 在没有记录时返回 ErrStockNotFound，用 errors.Is 判断，
// 而不是拿错误字符串去比——字符串是会变的，错误值不会。
func (h *Handler) ensureStock(skuID uint) error {
	_, err := h.stocks.Get(skuID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, inventory.ErrStockNotFound) {
		return err
	}

	// 给一个很大的数：这个库存是给普通下单链路用的，跟秒杀库存（seckill_items.total_stock）是两码事。
	_, err = h.stocks.Create(skuID, 100000)
	return err
}

// ensureUsers 创建 demo1、demo2…… 这批演示账号，返回可用的账号名列表。
//
// 为什么用「注册」而不是直接往数据库塞用户：注册接口会做密码哈希，
// 直接塞明文密码进去，之后是登不上的。复用注册逻辑就不会有这个问题。
// 账号已经存在时会返回 ErrUsernameTaken，这不是错误——说明上次已经建好了，跳过即可。
func (h *Handler) ensureUsers(count int) ([]string, error) {
	usernames := make([]string, 0, count)

	for i := 1; i <= count; i++ {
		username := fmt.Sprintf("demo%d", i)

		_, err := h.users.Register(username, demoPassword)
		if err != nil && !errors.Is(err, user.ErrUsernameTaken) {
			return nil, fmt.Errorf("注册 %s 失败: %w", username, err)
		}

		usernames = append(usernames, username)
	}

	return usernames, nil
}
