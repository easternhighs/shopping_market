package inventory

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册库存模块的路由。
func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.POST("/stocks", h.CreateStock)
	r.GET("/stocks/:sku_id", h.GetStock)
	r.POST("/stocks/:sku_id/deduct", h.DeductStock)
}
