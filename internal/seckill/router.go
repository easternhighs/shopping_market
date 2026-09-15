package seckill

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册秒杀模块的路由，所有接口都需要登录。
func RegisterRoutes(r *gin.RouterGroup, h *Handler) {
	r.POST("/seckill/activities", h.CreateActivity)
	r.GET("/seckill/activities", h.ListActivities)
	r.POST("/seckill/activities/:id/items", h.CreateItem)
	r.GET("/seckill/activities/:id/items", h.ListItems)
	r.POST("/seckill/items/:item_id/orders", h.Seckill)
}
