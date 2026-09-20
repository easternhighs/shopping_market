package metrics

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterObservabilityRoutes 挂上 6.3 新增的三条只读接口。
//
// 它们都在 /metrics 前缀下，已经被 ignoredPrefixes 排除在业务指标之外
// （见 middleware.go），所以演示页、压测报告随便轮询都不会污染压测口径。
//
//   - GET /metrics          累计值快照（原有接口，6.3 之后多了错误数、延迟、并发数）
//   - GET /metrics/history  采样历史（把累计值换算成速率后的每秒数字）
//   - GET /metrics/alerts   此刻在响的告警 + 最近的状态变化 + 每条规则的基线
func RegisterObservabilityRoutes(r gin.IRouter, sampler *Sampler, engine *Engine) {
	r.GET("/metrics/history", func(c *gin.Context) {
		samples := sampler.History()

		c.JSON(http.StatusOK, gin.H{
			"interval_seconds": sampler.Interval().Seconds(),
			"sample_count":     len(samples),
			"samples":          samples,
			"active_alerts":    engine.Active(),
			"events":           engine.Events(),
		})
	})

	r.GET("/metrics/alerts", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"active":      engine.Active(),
			"events":      engine.Events(),
			"rules":       engine.Status(),
			"server_time": time.Now(),
		})
	})
}
