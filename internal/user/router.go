package user

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册用户模块的路由。
func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.POST("/register", h.Register)
	r.POST("/login", h.Login)

	protected := r.Group("/")
	protected.Use(h.Auth)
	protected.GET("/me", h.Me)
}
