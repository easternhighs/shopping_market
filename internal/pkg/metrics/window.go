package metrics

import (
	"context"
	"log"
	"sync"
	"time"
)

// 采样里每个键的名字。集中写在这里，是为了让「谁提供这个数」和「谁读这个数」
// 用的是同一个字符串，避免出现 "qps" 和 "QPS" 这种对不上的低级问题。
const (
	MetricQPS               = "qps"                 // 每秒处理多少个请求（业务请求，不含 /demo、/metrics）
	MetricErrorRate         = "error_rate"          // 5xx 占请求数的百分比
	MetricClientErrorRate   = "client_error_rate"   // 4xx 占请求数的百分比（限流、参数错、重复请求都算）
	MetricRateLimitedQPS    = "rate_limited_qps"    // 每秒有多少请求被限流拒绝
	MetricSeckillQPS        = "seckill_qps"         // 每秒有多少个抢购请求打进来
	MetricSeckillSuccessQPS = "seckill_success_qps" // 每秒有多少订单真正落库成功
	MetricSeckillFailRate   = "seckill_fail_rate"   // 抢购里被判定为失败或重复的比例（百分数）
	MetricAvgLatencyMS      = "avg_latency_ms"      // 平均每个请求花多少毫秒
	MetricInflight          = "inflight"            // 此刻正在处理、还没返回的请求数
	MetricKafkaLag          = "kafka_lag"           // 消费组落后多少条消息没处理
	MetricPending           = "pending"             // 本进程已发出、但还没落库的在途订单数
	MetricAdaptiveLimit     = "adaptive_limit"      // 自适应限流此刻允许的每秒请求数
)

// Sample 是「某一个时刻的一组观测值」。
//
// 为什么需要它：Metrics 里存的是累计值（一共处理了多少个请求），累计值没法和
// 「平时」比较。采样器每隔固定时间记一次累计值，用两次的差除以间隔就得到速率，
// 再补上几个本来就是瞬时值的指标（并发数、Kafka 积压），就得到了这一个 Sample。
// 告警规则拿的就是 Sample.Values 里的数字。
type Sample struct {
	At     time.Time          `json:"at"`
	Values map[string]float64 `json:"values"`
}

// Sampler 是 6.3 的心跳：按固定节奏把累计指标换算成速率，再交给告警引擎。
//
// 没有它，系统只知道自己一共干了多少活；有了它，系统才知道自己「现在」是快了、
// 慢了、还是出错了。它跑在一个独立的 goroutine 里，不在请求路径上，
// 所以就算它算得慢一点，也不会拖慢抢购。
type Sampler struct {
	metrics  *Metrics
	engine   *Engine
	interval time.Duration
	capacity int
	logf     func(format string, args ...any)

	mu      sync.RWMutex
	extra   func() map[string]float64
	observe []func(Sample)
	samples []Sample
	prev    map[string]int64
	prevAt  time.Time
}

// NewSampler 创建采样器。
//
// interval 是采样间隔（本项目用 2 秒：既有足够的时间分辨率看出抖动，
// 又不会把采样历史撑爆）。capacity 是内存里保留多少个样本（900 个 × 2 秒 = 半小时）。
func NewSampler(m *Metrics, engine *Engine, interval time.Duration, capacity int) *Sampler {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if capacity <= 0 {
		capacity = 900
	}

	return &Sampler{
		metrics:  m,
		engine:   engine,
		interval: interval,
		capacity: capacity,
		logf:     log.Printf,
	}
}

// SetExtra 注册一个「补充数据源」。
//
// 采样器自己只认识 Metrics 里的计数器，但 Kafka LAG、在途订单数、自适应限流的
// 当前阈值这些值不在里面。用这个函数把它们挂进来，采样器就不需要认识 Kafka 和
// 限流器，两边保持解耦。
//
// 注意：这个函数会被采样器**同步调用**，所以里面不要做网络请求之类的慢操作，
// 否则会拖慢采样节奏（Kafka LAG 已经由 LagProbe 在自己的 goroutine 里刷新，见
// internal/pkg/kafka/lag.go）。
func (s *Sampler) SetExtra(fn func() map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra = fn
}

// SetObserver 注册一个「旁观者」：每采到一个新样本就被调用一次。
//
// 告警引擎走的是构造函数注入，这里再开一个口子，是因为有些用法是「消费」样本
// 而不是「判断」样本——比如自适应限流，它每个窗口都要拿平均延迟和错误率
// 重新算一次阈值，但它自己不是告警。
//
// 和 SetExtra 一样，回调是在采样 goroutine 里**同步执行**的，别在里面做慢操作。
func (s *Sampler) SetObserver(fn func(Sample)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe = append(s.observe, fn)
}

// Interval 返回采样间隔，报告工具需要用它来判断数据够不够密。
func (s *Sampler) Interval() time.Duration {
	return s.interval
}

// History 返回内存里保留的全部样本（拷贝，调用方改不到内部数据）。
func (s *Sampler) History() []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Sample, len(s.samples))
	copy(out, s.samples)
	return out
}

// Run 启动采样循环，直到 ctx 结束。
func (s *Sampler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collect()
		}
	}
}

// collect 采一次样：把累计值换算成速率，补上瞬时值，存进历史并交给告警引擎。
func (s *Sampler) collect() {
	now := time.Now()
	current := s.metrics.Snapshot()

	s.mu.Lock()
	if s.prevAt.IsZero() {
		// 第一次采样只用来打底：没有「上一次」，算不出速率。
		s.prev = current
		s.prevAt = now
		s.mu.Unlock()
		return
	}

	seconds := now.Sub(s.prevAt).Seconds()
	previous := s.prev
	s.prev = current
	s.prevAt = now
	extra := s.extra
	observers := s.observe
	s.mu.Unlock()

	if seconds <= 0 {
		return
	}

	values := make(map[string]float64, 16)
	values[MetricQPS] = rate(current, previous, "totalRequests", seconds)
	values[MetricRateLimitedQPS] = rate(current, previous, "rateLimited", seconds)
	values[MetricSeckillQPS] = rate(current, previous, "seckillRequests", seconds)
	values[MetricSeckillSuccessQPS] = rate(current, previous, "seckillSuccess", seconds)

	// 错误率和平均延迟的分母用「处理完成的请求数」，而不是「收到的请求数」。
	// 两者在任意一瞬间都不相等，分子（错误数、耗时）是在请求结束那一刻记的，
	// 分母也必须取同一时刻的数，比例才自洽；否则压测收尾的窗口会算出
	// 大于 100% 的错误率（详见 metrics.go 里 completed 字段的说明）。
	completed := delta(current, previous, "completedRequests")
	values[MetricErrorRate] = percent(delta(current, previous, "serverErrors"), completed)
	values[MetricClientErrorRate] = percent(delta(current, previous, "clientErrors"), completed)
	values[MetricAvgLatencyMS] = average(delta(current, previous, "latencyMillis"), completed)

	seckill := delta(current, previous, "seckillRequests")
	seckillBad := delta(current, previous, "seckillFailure") + delta(current, previous, "seckillDuplicate")
	values[MetricSeckillFailRate] = percent(seckillBad, seckill)

	values[MetricInflight] = float64(current["inflight"])

	if extra != nil {
		for key, value := range extra() {
			values[key] = value
		}
	}

	sample := Sample{At: now, Values: values}

	s.mu.Lock()
	s.samples = append(s.samples, sample)
	if len(s.samples) > s.capacity {
		s.samples = s.samples[len(s.samples)-s.capacity:]
	}
	s.mu.Unlock()

	if s.engine != nil {
		s.engine.Observe(sample)
	}

	for _, observer := range observers {
		if observer != nil {
			observer(sample)
		}
	}
}

// delta 返回某个累计值在这段时间里增长了多少。
// 计数器只会增加，理论上不会出现负数；真出现了（比如进程重启后读到了旧快照）
// 就按 0 处理，免得算出一个负的 QPS。
func delta(current, previous map[string]int64, key string) int64 {
	d := current[key] - previous[key]
	if d < 0 {
		return 0
	}
	return d
}

// rate 把「增长了多少」换算成「每秒多少」。
func rate(current, previous map[string]int64, key string, seconds float64) float64 {
	return float64(delta(current, previous, key)) / seconds
}

// percent 算「part 占 total 的百分之多少」。
// total 为 0 时（这段时间一个请求都没有）返回 0：没有请求就没有错误率，
// 不能返回 100 或者 NaN 把告警系统带偏。
func percent(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// average 算平均值，分母为 0 时返回 0。
func average(sum int64, count int64) float64 {
	if count <= 0 {
		return 0
	}
	return float64(sum) / float64(count)
}
