package payment

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册支付模块的路由，所有接口都需要登录。
func RegisterRoutes(r *gin.RouterGroup, h *Handler) {
	r.POST("/payments/:order_id/callback", h.Callback)
	r.GET("/payments/:order_id", h.Get)
}
