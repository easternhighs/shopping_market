package order

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册订单模块的路由，所有接口都需要登录。
func RegisterRoutes(r *gin.RouterGroup, h *Handler) {
	r.POST("/orders", h.Place)
	r.GET("/orders/:id", h.Get)
	r.POST("/orders/:id/pay", h.Pay)
	r.POST("/orders/:id/cancel", h.Cancel)
}
