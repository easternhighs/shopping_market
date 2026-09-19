package metrics

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ignoredPrefixes 是不计入「业务请求数」的路径前缀。
//
// 为什么要专门把它们排除掉：演示控制台页面活着的时候，每一秒都在轮询
// /demo/data 和 /metrics，如果这些请求也算进 totalRequests，
// 压测时看到的「总请求数」里就混进了一堆演示页自己转出来的流量，
// 这个数字不再代表真实业务压力。演示页要展示的是「压测打了多少」，
// 不是「演示页自己转了多少圈」。
//
// 注意：只影响这一个计数器。秒杀的成功/失败/重复/限流这些指标是由业务层
// 主动上报的（见 seckill.Service.SetMetricsObserver），压根不经过这里，
// 所以就算有人拿 /demo 页面去点，也不会污染压测口径的秒杀指标。
var ignoredPrefixes = []string{"/demo", "/metrics"}

// Middleware 是 Gin 的全局指标中间件。
// 每个请求进来时，先记录一次请求总数，再放行给后续接口。
func Middleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !shouldIgnore(c.Request.URL.Path) {
			m.ObserveRequest()
		}
		c.Next()
	}
}

// shouldIgnore 判断某个路径是不是「观测流量」，不该算进业务请求数。
func shouldIgnore(path string) bool {
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
