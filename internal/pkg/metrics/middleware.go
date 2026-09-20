package metrics

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ignoredPrefixes 是不计入「业务请求」的路径前缀。
//
// 为什么要专门把它们排除掉：演示控制台页面活着的时候，每一秒都在轮询
// /demo/data 和 /metrics，如果这些请求也算进 totalRequests，
// 压测时看到的「总请求数」里就混进了一堆演示页自己转出来的流量，
// 这个数字不再代表真实业务压力。演示页要展示的是「压测打了多少」，
// 不是「演示页自己转了多少圈」。
//
// 6.3 又多了两个理由：这两条路径的耗时也会被算进「平均延迟」，
// 而演示页的轮询要查 MySQL、查 Redis，本来就比 /health 慢，
// 混进去会把平均延迟整体抬高，让自适应限流误判「系统很忙」。
//
// 注意：只影响 HTTP 这一层统计。秒杀的成功/失败/重复/限流这些指标是由业务层
// 主动上报的（见 seckill.Service.SetMetricsObserver），压根不经过这里，
// 所以就算有人拿 /demo 页面去点，也不会污染压测口径的秒杀指标。
//
// 阶段 7 又多了 /shop：那是给用户看的独立前台，它每秒都在轮询 /shop/data 刷新库存。
// 同一个道理——用户开着前台页面，不代表压测口径里的「业务请求」变多了。
var ignoredPrefixes = []string{"/demo", "/metrics", "/shop"}

// Middleware 是 Gin 的全局指标中间件。
//
// 它在每个请求上做四件事，全部都是往内存里的计数器加一加，没有 IO：
//  1. 请求总数 +1（业务请求才计）；
//  2. 并发数 +1，请求结束时 -1（用来观察「此刻有多少请求正在处理」）；
//  3. 把这次请求的耗时累加进 latencyMillis（用来算平均延迟）；
//  4. 按状态码把 5xx / 4xx 分别计数（用来算错误率）。
//
// 这些数字加起来的价格是每次请求几个原子加法，纳秒级；
// 6.3 落地后重新压测过，QPS 没有可见下降（见 docs/build.md 的实测记录）。
func Middleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		if shouldIgnore(c.Request.URL.Path) {
			c.Next()
			return
		}

		start := time.Now()
		m.ObserveRequest()
		m.Enter()

		c.Next()

		m.Leave(c.Writer.Status(), time.Since(start))
	}
}

// shouldIgnore 判断某个路径是不是「观测流量」，不该算进业务指标。
func shouldIgnore(path string) bool {
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
