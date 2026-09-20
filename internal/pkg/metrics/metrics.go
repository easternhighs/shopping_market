package metrics

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics 保存服务运行期间的一些基础指标。
// 它相当于汽车仪表盘：只看几个关键数字，就能大致知道服务忙不忙、有没有异常。
//
// 这里所有字段存的都是「从进程启动到现在的累计值」：totalRequests 是「一共收到过
// 多少个请求」，而不是「现在每秒多少个请求」。累计值本身不会告诉你快慢，要判断
// 「现在是不是异常」得先把它换算成速率——这一步由 Sampler 完成（见 window.go），
// 换算出来的结果再交给告警引擎判断（见 alert.go）。
type Metrics struct {
	totalRequests    atomic.Int64
	rateLimited      atomic.Int64
	seckillRequests  atomic.Int64
	seckillSuccess   atomic.Int64
	seckillFailure   atomic.Int64
	seckillDuplicate atomic.Int64

	// 下面四项是 6.3 新增的，为的是让「错误率」和「平均延迟」算得出来：
	//   - serverErrors：返回 5xx 的请求数（服务端真的出故障了）；
	//   - clientErrors：返回 4xx 的请求数（限流、参数错、重复请求等，属于正常拒绝，
	//     不能和 5xx 混在一起，否则限流一开「错误率」就爆表，那是自己吓自己）；
	//   - latencyMillis：所有请求耗时之和（毫秒），除以请求数就是平均延迟；
	//   - inflight：此刻还在处理、还没返回的请求数，也就是「并发量」，采样时直接读；
	//   - completed：已经处理完的请求数。它是「错误率」和「平均延迟」的分母，
	//     单独计一个，而不是复用 totalRequests。
	//     为什么：totalRequests 数的是「请求进来」，completed 数的是「请求处理完」，
	//     同一时刻这两个数并不相等（总有请求在路上）。如果拿 totalRequests 当分母，
	//     压测收尾那一个采样窗口会算出「4xx 占 100.43%」这种大于 100% 的怪数字——
	//     分子里混进了上一秒进来、这一秒才结束的请求，分母却没算它们。
	//     分子和分母都来自「处理完成」这一个时刻，比例就永远不可能超过 100%。
	serverErrors  atomic.Int64
	clientErrors  atomic.Int64
	latencyMillis atomic.Int64
	inflight      atomic.Int64
	completed     atomic.Int64
}

// NewMetrics 创建指标对象。
func NewMetrics() *Metrics {
	return &Metrics{}
}

// ObserveRequest 表示收到一个普通请求。
func (m *Metrics) ObserveRequest() {
	m.totalRequests.Add(1)
}

// Enter 表示一个请求开始处理，并发数加一。
func (m *Metrics) Enter() {
	m.inflight.Add(1)
}

// Leave 表示一个请求处理完毕，并发数减一，同时记下它的耗时和状态码。
//
// status 是 HTTP 状态码，cost 是这个请求花了多久。
// 这两个值会分别累加成「错误率」和「平均延迟」，是判断系统有没有退化的两条主线索。
func (m *Metrics) Leave(status int, cost time.Duration) {
	m.inflight.Add(-1)
	m.completed.Add(1)
	m.latencyMillis.Add(cost.Milliseconds())

	switch {
	case status >= http.StatusInternalServerError:
		m.serverErrors.Add(1)
	case status >= http.StatusBadRequest:
		m.clientErrors.Add(1)
	}
}

// ObserveRateLimited 表示有一个请求被限流拒绝。
func (m *Metrics) ObserveRateLimited() {
	m.rateLimited.Add(1)
}

// ObserveSeckill 表示有一个秒杀请求进入统计。
// kind 可以是 request、success、failure、duplicate。
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

// Inflight 返回此刻还在处理中的请求数。
func (m *Metrics) Inflight() int64 {
	return m.inflight.Load()
}

// Snapshot 返回当前所有累计指标的只读快照。
//
// 返回 map 而不是结构体，是为了让采样器可以按名字遍历：
// 采样器不需要知道有哪些指标，只要逐个相减就能算出速率。
func (m *Metrics) Snapshot() map[string]int64 {
	snapshot := make(map[string]int64)

	snapshot["totalRequests"] = m.totalRequests.Load()
	snapshot["rateLimited"] = m.rateLimited.Load()
	snapshot["seckillRequests"] = m.seckillRequests.Load()
	snapshot["seckillSuccess"] = m.seckillSuccess.Load()
	snapshot["seckillFailure"] = m.seckillFailure.Load()
	snapshot["seckillDuplicate"] = m.seckillDuplicate.Load()
	snapshot["serverErrors"] = m.serverErrors.Load()
	snapshot["clientErrors"] = m.clientErrors.Load()
	snapshot["latencyMillis"] = m.latencyMillis.Load()
	snapshot["inflight"] = m.inflight.Load()
	snapshot["completedRequests"] = m.completed.Load()

	return snapshot
}

// Handler 返回一个 HTTP 接口，访问时输出当前的累计指标 JSON。
//
// 它输出的是「一共」多少，不是「每秒」多少；要看每秒多少、有没有告警，
// 请访问 /metrics/history 和 /metrics/alerts（见 http.go）。
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
}
