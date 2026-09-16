package metrics

import (
	"github.com/gin-gonic/gin"
)

// Middleware 是 Gin 的全局指标中间件。
// 每个请求进来时，先记录一次请求总数，再放行给后续接口。
func Middleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		m.ObserveRequest()
		c.Next()
	}
}
