package seckill

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// RegisterInternalRoutes 注册只给兄弟服务（shop / console）使用的秒杀接口。
//
// 它们不挂登录中间件：调用方是内网里的服务，不是浏览器。
// 隔离方式仍然是「网关不认识 /internal 前缀」，真上线要加内网监听或服务间认证。
func RegisterInternalRoutes(r *gin.Engine, h *Handler) {
	r.GET("/internal/seckill/activities", h.ListActivities)
	r.POST("/internal/seckill/activities", h.CreateActivity)
	r.GET("/internal/seckill/activities/:id/items", h.ListItems)
	r.POST("/internal/seckill/activities/:id/items", h.CreateItem)
	r.GET("/internal/seckill/items/:item_id/stock", h.RemainingStockInternal)
	r.POST("/internal/seckill/items/:item_id/warmup", h.WarmUpStockInternal)
	r.GET("/internal/seckill/pending", h.PendingInternal)
	r.GET("/internal/seckill/users/:user_id/orders", h.ListUserOrdersInternal)
	r.GET("/internal/seckill/config", h.ConfigInternal)
}

// RemainingStockInternal 处理 GET /internal/seckill/items/:item_id/stock。
func (h *Handler) RemainingStockInternal(c *gin.Context) {
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "秒杀商品 ID 格式错误"})
		return
	}

	remaining, ready, err := h.service.RemainingStock(c.Request.Context(), uint(itemID))
	if err != nil {
		c.JSON(seckillErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"remaining_stock": remaining,
		"stock_ready":     ready,
	})
}

// WarmUpStockInternal 处理 POST /internal/seckill/items/:item_id/warmup。
func (h *Handler) WarmUpStockInternal(c *gin.Context) {
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "秒杀商品 ID 格式错误"})
		return
	}

	if err := h.service.WarmUpStock(c.Request.Context(), uint(itemID)); err != nil {
		c.JSON(seckillErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PendingInternal 处理 GET /internal/seckill/pending。
func (h *Handler) PendingInternal(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"pending": h.service.Pending()})
}

// ListUserOrdersInternal 处理 GET /internal/seckill/users/:user_id/orders。
func (h *Handler) ListUserOrdersInternal(c *gin.Context) {
	userID, err := strconv.ParseUint(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户 ID 格式错误"})
		return
	}

	orders, err := h.service.ListUserOrders(uint(userID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

// ConfigInternal 处理 GET /internal/seckill/config。
//
// 只暴露页面需要的配置，不要把整个 Service 的状态直接吐出去。
func (h *Handler) ConfigInternal(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"pay_timeout_seconds": int64(h.service.PayTimeout().Seconds()),
	})
}
