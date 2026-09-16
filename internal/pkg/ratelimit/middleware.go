package ratelimit

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit 是 Gin 限流中间件。
// 它会在请求进入具体接口之前先问 Limiter：“这个请求能不能放行？”
// 不能放行就直接返回 429，不给后面的业务代码增加压力。
func RateLimit(limiter Limiter, limit int, window time.Duration, onLimited ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed, err := limiter.Allow(c.Request.Context(), c.ClientIP(), limit, window)
		if err != nil {
			// 使用c.Abort()返回异常数据，将错误信息通过gin.H包装成error键值对提交,通过return终止程序
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "限流服务异常"})
			return
		}
		if !allowed {
			for _, fn := range onLimited {
				if fn != nil {
					fn()
				}
			}
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "请求过多"})
			return
		}

		c.Next()
	}
}
