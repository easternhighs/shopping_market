package metrics

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// Metrics 保存服务运行期间的一些基础指标。
// 它相当于汽车仪表盘：只看几个关键数字，就能大致知道服务忙不忙、有没有异常。
type Metrics struct {
	totalRequests    atomic.Int64
	rateLimited      atomic.Int64
	seckillRequests  atomic.Int64
	seckillSuccess   atomic.Int64
	seckillFailure   atomic.Int64
	seckillDuplicate atomic.Int64
}

// NewMetrics 创建指标对象。
func NewMetrics() *Metrics {
	return &Metrics{}
}

// ObserveRequest 表示收到一个普通请求。
func (m *Metrics) ObserveRequest() {
	m.totalRequests.Add(1)
}

// ObserveRateLimited 表示有一个请求被限流拒绝。
func (m *Metrics) ObserveRateLimited() {
	m.rateLimited.Add(1)
}

// ObserveSeckill 表示有一个秒杀请求进入统计。
// kind 可以是 success、failure、duplicate。
func (m *Metrics) ObserveSeckill(kind string) {
	switch kind {
	case "request":
		m.seckillRequests.Add(1)
	case "success":
		m.seckillSuccess.Add(1)
	case "failure":
		m.seckillFailure.Add(1)
	case "duplicate":
		m.seckillDuplicate.Add(1)
	}
}

// Snapshot 返回当前所有指标的只读快照。
func (m *Metrics) Snapshot() map[string]int64 {
	snapshot := make(map[string]int64)

	snapshot["totalRequests"] = m.totalRequests.Load()
	snapshot["rateLimited"] = m.rateLimited.Load()
	snapshot["seckillRequests"] = m.seckillRequests.Load()
	snapshot["seckillSuccess"] = m.seckillSuccess.Load()
	snapshot["seckillFailure"] = m.seckillFailure.Load()
	snapshot["seckillDuplicate"] = m.seckillDuplicate.Load()

	return snapshot
}

// Handler 返回一个 HTTP 接口，访问时输出当前指标 JSON。
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
}
