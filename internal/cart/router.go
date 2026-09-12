package cart

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册购物车模块的路由，所有接口都需要登录。
func RegisterRoutes(r *gin.RouterGroup, h *Handler) {
	r.POST("/cart/items", h.AddItem)
	r.GET("/cart/items", h.List)
	r.PATCH("/cart/items/:item_id", h.UpdateQuantity)
	r.DELETE("/cart/items/:item_id", h.RemoveItem)
}
