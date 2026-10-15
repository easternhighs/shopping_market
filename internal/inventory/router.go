package inventory

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册库存模块的路由。
func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.POST("/stocks", h.CreateStock)
	r.GET("/stocks/:sku_id", h.GetStock)
	r.POST("/stocks/:sku_id/deduct", h.DeductStock)

	// 阶段 16：订单服务跨进程扣库存走的内部接口。
	//
	// /internal 这个前缀是刻意选的：
	//   - 网关的路由表（internal/gateway/route.go）里【没有】它，
	//     所以从外面访问是 404 —— 它本来就只该被内网里的兄弟服务调；
	//   - 真上生产时还要再加一层「只监听内网 / 只允许白名单 IP」，
	//     这里只是靠「网关不认识这个前缀」做隔离，属于演示级别的防护。
	r.POST("/internal/stocks/deduct", h.DeductBatchStock)
}
