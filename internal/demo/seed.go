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

// 演示数据的固定名字与密码。反复点「一键准备」时靠这些固定值做幂等判断，
// 所以商品名、规格名、账号名都不能带随机后缀。
const (
	demoProductName  = "演示秒杀商品"
	demoProductDesc  = "演示控制台一键准备数据时自动创建的商品，秒杀活动就挂在它的 SKU 上。"
	demoProductPrice = 199.0
	demoSpec         = "标准版"
	demoSKUPrice     = 9.9
	demoPassword     = "demo1234"
)

// seedRequest 是「一键准备」接口的请求体，两个字段都可以不填，不填就用默认值。
type seedRequest struct {
	// UserCount 要准备几个演示账号（demo1、demo2……）。
	UserCount int `json:"user_count"`
	// TotalStock 这次秒杀活动放多少件库存。
	TotalStock int `json:"total_stock"`
}

// Seed 处理 POST /demo/seed：把演示要用的数据一次性准备好。
//
// 整个过程分 7 步，每一步都先说「为什么需要它」：
//
//	第 1 步 商品：秒杀商品要挂在一个真实存在的商品下面，不然数据是悬空的；
//	第 2 步 SKU：用户真正购买的是 SKU（具体规格），秒杀商品记录里存的是 SKU ID；
//	第 3 步 普通库存：秒杀本身不用它，但少了它数据看起来不完整，顺手补上；
//	第 4 步 秒杀活动：定一个「5 分钟前开始、2 小时后结束」的时间窗，方便随到随抢；
//	第 5 步 秒杀商品：把 SKU 挂进活动，指定总库存和每人限购；
//	第 6 步 预热 Redis：把商品信息、活动时间窗和剩余库存写进 Redis，
//	        页面一打开就能看到库存进度条，第一次抢购也不用走冷路径；
//	第 7 步 演示账号：创建 demo1、demo2…… 并用统一的密码，省掉现场手填注册信息。
//
// 幂等性：商品、SKU、库存、账号都是「有就复用，没有才建」；
// 只有活动和秒杀商品每次都是新建的，因为每次演示都想要一个「库存满、时间还没过」的新活动。
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
	if req.TotalStock <= 0 {
		req.TotalStock = 100
	}

	ctx := c.Request.Context()

	// 第 1 步：商品。
	prod, err := h.ensureProduct()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建商品失败: " + err.Error()})
		return
	}

	// 第 2 步：SKU。
	sku, err := h.ensureSKU(prod.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建 SKU 失败: " + err.Error()})
		return
	}

	// 第 3 步：普通库存。
	if err := h.ensureStock(sku.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建库存失败: " + err.Error()})
		return
	}

	// 第 4 步：秒杀活动。开始时间往前挪 5 分钟，避免「刚建好却提示还没开始」。
	now := time.Now()
	activity, err := h.seckill.CreateActivity(
		"演示秒杀活动 "+now.Format("01-02 15:04"),
		now.Add(-5*time.Minute),
		now.Add(2*time.Hour),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建秒杀活动失败: " + err.Error()})
		return
	}

	// 第 5 步：秒杀商品。
	item, err := h.seckill.CreateItem(activity.ID, sku.ID, demoSKUPrice, req.TotalStock, 1)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建秒杀商品失败: " + err.Error()})
		return
	}

	// 第 6 步：预热 Redis。
	if err := h.seckill.WarmUpStock(ctx, item.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "预热 Redis 失败: " + err.Error()})
		return
	}

	// 第 7 步：演示账号。
	usernames, err := h.ensureUsers(req.UserCount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建演示账号失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"product_id":     prod.ID,
		"sku_id":         sku.ID,
		"activity_id":    activity.ID,
		"item_id":        item.ID,
		"activity_name":  activity.Name,
		"total_stock":    item.TotalStock,
		"per_user_limit": item.PerUserLimit,
		"price":          item.Price,
		"start_at":       activity.StartAt,
		"end_at":         activity.EndAt,
		"users":          usernames,
		"password":       demoPassword,
		"hint":           fmt.Sprintf("用 %s 里的任意账号 + 密码 %s 登录，然后点「立即抢购」", usernames[0], demoPassword),
	})
}

// ensureProduct 找到演示商品，找不到才建。
// 商品模块没有「按名字查」的接口，所以用 List 全量取回来在内存里比一遍——
// 演示数据的数据量很小，这样写最省事，也不用为此再给商品模块加接口。
func (h *Handler) ensureProduct() (*product.Product, error) {
	products, err := h.products.List()
	if err != nil {
		return nil, err
	}

	for i := range products {
		if products[i].Name == demoProductName {
			return &products[i], nil
		}
	}

	return h.products.Create(demoProductName, demoProductDesc, demoProductPrice)
}

// ensureSKU 找到「演示商品 + 标准版」这个 SKU，找不到才建。
func (h *Handler) ensureSKU(productID uint) (*product.SKU, error) {
	sku, err := h.products.FindSKUByProductAndSpec(productID, demoSpec)
	if err != nil {
		return nil, err
	}
	if sku != nil {
		return sku, nil
	}

	return h.products.CreateSKU(productID, demoSpec, demoSKUPrice)
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
