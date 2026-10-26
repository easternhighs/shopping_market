package product

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册商品模块的路由。
func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.POST("/products", h.Create)
	r.GET("/products", h.List)
	r.GET("/products/:id", h.Get)
	r.DELETE("/products/:id", h.Delete)
	r.POST("/products/:id/skus", h.CreateSKU)

	// 内部接口跟着商品模块一起注册：谁提供商品路由，谁就负责给兄弟服务提供取数接口。
	RegisterInternalRoutes(r, h)
}
