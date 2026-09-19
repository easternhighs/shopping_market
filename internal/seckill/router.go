package seckill

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册秒杀模块的路由，所有接口都需要登录。
//
// 为什么要分两个组传进来：
// 抢购接口把限流放进了 Redis 脚本里、按用户计数，如果外面再套一层 IP 限流中间件，
// 那次中间件本身又是一次独立的 Redis 往返，正好抵消掉这一轮的优化。
// 所以管理类接口（建活动、查活动、建商品）照旧走带限流的组，
// 抢购接口单独注册到不带中间件的组上。
func RegisterRoutes(limited *gin.RouterGroup, plain *gin.RouterGroup, h *Handler) {
	limited.POST("/seckill/activities", h.CreateActivity)
	limited.GET("/seckill/activities", h.ListActivities)
	limited.POST("/seckill/activities/:id/items", h.CreateItem)
	limited.GET("/seckill/activities/:id/items", h.ListItems)

	plain.POST("/seckill/items/:item_id/orders", h.Seckill)
}
