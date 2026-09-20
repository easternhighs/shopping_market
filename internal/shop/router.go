package shop

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册前台的四条路由。
//
// auth 由外部传进来（main.go 传的是 user 模块的 JWT 中间件），而不是在包内 import user：
// 这样前台包不需要认识「登录」是怎么实现的，只要求「有一个能把 userID 放进上下文的中间件」。
// 好处是以后换鉴权方式（换成 Cookie、换成网关透传）只要换一个参数，这个包不用动。
//
// 页面本身不挂鉴权：得先能打开页面，才谈得上在里面登录。
// 只有 /shop/my 需要登录，因为订单是私密数据。
func RegisterRoutes(r *gin.Engine, h *Handler, auth gin.HandlerFunc) {
	r.GET("/shop", h.Page)
	r.GET("/shop/seckill", h.SeckillPage)
	r.GET("/shop/data", h.Data)
	r.GET("/shop/my", auth, h.MyOrders)
}
