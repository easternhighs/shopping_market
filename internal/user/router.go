package user

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册用户模块的路由。
//
// auth 这个中间件由调用方传进来，模块自己不自带（阶段 14 的改动）。
// 原因是同一个模块会被两种方式启动，而它们需要的鉴权方式不一样：
//   - cmd/server（单体）：单独启动时只验 JWT；躲在网关后面时优先信 X-User-Id；
//   - cmd/user（独立进程）：它的定位就是跑在网关后面。
//
// 让调用方各自传一个进来，模块本身就不用关心「我现在跑在哪儿」。
//
// 这和 internal/shop/router.go 的做法一样：谁提供鉴权，由外面决定。
func RegisterRoutes(r *gin.Engine, h *Handler, auth gin.HandlerFunc) {
	r.POST("/register", h.Register)
	r.POST("/login", h.Login)

	protected := r.Group("/")
	protected.Use(auth)
	protected.GET("/me", h.Me)
}
