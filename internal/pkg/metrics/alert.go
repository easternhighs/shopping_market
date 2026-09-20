package metrics

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// 偏离方向。
const (
	// DirectionAbove：指标「高于」某个水平才算异常（QPS 突增、错误率上升、LAG 堆积都属这一类）。
	DirectionAbove = "above"
	// DirectionBelow：指标「低于」某个水平才算异常（流量突然掉到没有，属于这一类）。
	DirectionBelow = "below"
)

// Rule 是一条告警规则：看哪个指标、往哪个方向偏、偏到什么程度才算异常。
//
// 判断分三层，这三层就是「统计方法做告警」的全部套路：
//
//  1. 绝对门槛（Floor）——先过这一关，否则连看的必要都没有。
//     它的作用是压住低流量时的误报：凌晨没人访问，QPS 从每秒 2 个掉到 0 个，
//     用数学算偏离非常大，可那不是故障。LAG 也一样，堆了 3 条消息不值得报警。
//
//  2. 相对基线（Sigma）——拿最近 Window 个样本算平均值和标准差，
//     只有偏离基线 Sigma 倍标准差，才算「和平时不一样」。
//     为什么不直接写死阈值（比如「QPS > 5000 就报警」）：同一套规则要能适应
//     压测（每秒上万）和日常（每秒几个）两种完全不同的量级，写死的话要么平时
//     一直误报、要么压测时该报的报不出来。用「和平时比」就没有这个问题。
//
//  3. 连续确认（Consecutive）——连续多少个采样窗口都越线才真正告警。
//     网络抖动、GC 停顿都会让某一个窗口的数字毛刺一下，连续几秒都越线才是真趋势。
//     这是降低误报最有效的一招，代价是告警会晚几秒。
type Rule struct {
	Name        string
	Metric      string
	Direction   string
	Floor       float64
	Sigma       float64
	Window      int
	MinSamples  int
	Consecutive int

	// Reason 把「当前值 / 基线 / 波动 / 门槛」翻译成一句人话，告诉看日志的人
	// 发生了什么、接下来该看哪里。告警如果只报「QPS 异常」，对排查没有任何帮助。
	Reason func(cur, baseline, sigma, threshold float64) string
}

// Alert 是一条正在响的告警。
type Alert struct {
	Rule           string    `json:"rule"`
	Metric         string    `json:"metric"`
	Direction      string    `json:"direction"`
	Value          float64   `json:"value"`
	Baseline       float64   `json:"baseline"`
	Sigma          float64   `json:"sigma"`
	Threshold      float64   `json:"threshold"`
	Peak           float64   `json:"peak"`
	Since          time.Time `json:"since"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	Reason         string    `json:"reason"`
}

// AlertEvent 是告警的一次「状态变化」，要么是开始响，要么是恢复。
//
// 为什么连恢复也要记：只保留「正在响」的话，等你去翻记录时可能刚好恢复了，
// 就什么都看不到。压测报告靠这份记录回答「刚才那轮压测系统自己发现了什么」。
type AlertEvent struct {
	Alert
	Kind            string    `json:"kind"` // firing（开始告警）/ resolved（恢复）
	At              time.Time `json:"at"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
}

// RuleStatus 是一条规则「此刻的体检数据」，给演示页和压测报告看：
// 基线是多少、攒了多少样本、现在是不是在响。系统自己报异常的时候，
// 光说「异常」没用，得能同时给出平时是多少，才让人信服。
type RuleStatus struct {
	Name        string  `json:"name"`
	Metric      string  `json:"metric"`
	Direction   string  `json:"direction"`
	Floor       float64 `json:"floor"`
	Sigma       float64 `json:"sigma"`
	Consecutive int     `json:"consecutive"`
	Baseline    float64 `json:"baseline"`
	StdDev      float64 `json:"stddev"`
	Samples     int     `json:"samples"`
	Streak      int     `json:"streak"`
	Active      bool    `json:"active"`

	// Current 是最近一个采样窗口里这条规则看到的数值，HasCurrent 表示有没有采到过。
	// 演示页要把它和基线并排显示：「现在 5000，平时 0」比「正在告警」有说服力得多。
	Current    float64 `json:"current"`
	HasCurrent bool    `json:"has_current"`
}

// ruleState 是一条规则的运行状态。
type ruleState struct {
	rule Rule

	// values 是「正常时期」的样本窗口，基线的均值和标准差就是从它算出来的。
	//
	// 关键设计：异常期间的数值**不进这个窗口**。
	// 因为压测一跑十分钟，如果异常值也进窗口，基线会被一点点抬高，
	// 几分钟后「一万 QPS」就成了新的「平时」——异常把自己藏起来了。
	// 所以规则一旦越线，基线就冻结在异常之前，直到恢复正常才继续学习。
	values []float64

	streak   int // 连续越线了几个窗口
	okStreak int // 连续正常了几个窗口（用于判断恢复）
	active   bool
	peak     float64
	since    time.Time
	last     Alert

	// current 是最新一个采样窗口的数值。不管正不正常都记，因为演示页要展示
	// 「离门槛还有多远」——正不正常是结论，当前值是这个结论的论据。
	current    float64
	hasCurrent bool
}

// Engine 是告警引擎：接收采样，维护每条规则的状态，在状态变化时记一条日志。
//
// 它不负责「通知」（发邮件、发钉钉之类），本项目只需要两件事：
// 状态变化时打一行日志，以及把结果暴露给 /metrics/alerts 和演示页。
type Engine struct {
	mu     sync.Mutex
	states []*ruleState
	events []AlertEvent
	limit  int
	logf   func(format string, args ...any)
}

// NewEngine 创建告警引擎。logf 传 nil 时用标准日志。
func NewEngine(rules []Rule, logf func(format string, args ...any)) *Engine {
	if logf == nil {
		logf = log.Printf
	}

	engine := &Engine{limit: 200, logf: logf}
	for _, rule := range rules {
		if rule.Window <= 0 {
			rule.Window = 30
		}
		if rule.MinSamples <= 0 {
			rule.MinSamples = 10
		}
		if rule.Consecutive <= 0 {
			rule.Consecutive = 3
		}
		if rule.Sigma <= 0 {
			rule.Sigma = 4
		}
		engine.states = append(engine.states, &ruleState{rule: rule, peak: math.Inf(-1)})
	}

	return engine
}

// Observe 吃进一个采样，更新所有规则的状态。
func (e *Engine) Observe(sample Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, state := range e.states {
		value, ok := sample.Values[state.rule.Metric]
		if !ok {
			continue
		}

		state.current = value
		state.hasCurrent = true

		alert, abnormal := state.evaluate(value, sample.At)

		if abnormal {
			state.streak++
			state.okStreak = 0

			if !state.active && state.streak >= state.rule.Consecutive {
				state.active = true
				state.since = sample.At
				state.peak = value
				state.last = alert
				e.record(AlertEvent{Alert: alert, Kind: "firing", At: sample.At})
				e.logf("[告警] %s：%s", alert.Rule, alert.Reason)
				continue
			}

			if state.active {
				// peak 记的是「从开始告警到现在最极端的那一下」，不是最后一个窗口的值。
				// 恢复的时候要能说出「最坏到过多少」，这个数只有累积下来才有。
				if state.rule.Direction == DirectionAbove {
					state.peak = math.Max(state.peak, value)
				} else {
					state.peak = math.Min(state.peak, value)
				}
				alert.Since = state.since
				alert.Peak = state.peak
				state.last = alert
			}
			continue
		}

		// 恢复正常：连续几个正常窗口才解除，避免一条规则在门槛附近来回抖、
		// 把日志刷成一片「告警 / 恢复」。
		state.streak = 0
		state.okStreak++

		if state.active && state.okStreak >= 2 {
			state.active = false

			duration := sample.At.Sub(state.since).Seconds()

			// 恢复事件里也要带上「期间最坏到过多少」，所以显式把累积的 peak 填进去。
			// 不填的话事件里的 Peak 会是最后一个窗口的值：日志写「峰值 5294」、
			// 接口却报「峰值 586」，同一个事件两个数字，看的人会以为哪里算错了。
			last := state.last
			last.Peak = state.peak
			last.ElapsedSeconds = duration

			e.record(AlertEvent{
				Alert:           last,
				Kind:            "resolved",
				At:              sample.At,
				DurationSeconds: duration,
			})
			e.logf("[告警恢复] %s：持续 %.0f 秒，峰值 %.1f，已回到基线附近",
				last.Rule, duration, state.peak)
			state.peak = math.Inf(-1)
		}

		// 只有正常值才喂给基线窗口（异常值会把基线抬走，等于把异常藏起来）。
		state.push(value)
	}
}

// evaluate 判断一条规则此刻算不算异常，并组装出告警内容。
func (s *ruleState) evaluate(value float64, at time.Time) (Alert, bool) {
	rule := s.rule

	// 第一关：绝对门槛。没过就算数学上偏离再大也不看。
	if rule.Direction == DirectionAbove && value < rule.Floor {
		return Alert{}, false
	}

	// 样本还不够就先别急着下结论，先攒够「平时」的样子。
	if len(s.values) < rule.MinSamples {
		return Alert{}, false
	}

	baseline, sigma := meanStdDev(s.values)

	var threshold float64
	switch rule.Direction {
	case DirectionAbove:
		threshold = baseline + rule.Sigma*sigma
		// 有效门槛取「基线 + N 倍波动」和绝对下限里更严的那个。
		//
		// 为什么必须取较大值：服务刚启动、或者平时压根没流量的时候，基线是 0，
		// 「基线 + N 倍波动」也是 0。如果只拿它当门槛，告警文案会写成
		// 「越过门槛 0/s」，看着就像随便来点流量都算异常；而且这个 0 和
		// /metrics/alerts 里展示的门槛对不上，查问题时容易看糊涂。
		// 取了较大值之后，「比较用的值」和「写给人看的值」就是同一个。
		if rule.Floor > threshold {
			threshold = rule.Floor
		}
		if value <= threshold {
			return Alert{}, false
		}
	case DirectionBelow:
		// 「掉下去」的规则要额外看底线：只有平时确实有流量（基线高于 Floor），
		// 「现在掉到很低」才有意义。否则深夜本来就没流量，天天报警。
		if baseline < rule.Floor {
			return Alert{}, false
		}
		threshold = baseline - rule.Sigma*sigma
		if value >= threshold {
			return Alert{}, false
		}
	default:
		return Alert{}, false
	}

	reason := ""
	if rule.Reason != nil {
		reason = rule.Reason(value, baseline, sigma, threshold)
	} else {
		reason = fmt.Sprintf("%s 当前 %.2f，平时约 %.2f（波动 ±%.2f），越过门槛 %.2f",
			rule.Metric, value, baseline, sigma, threshold)
	}

	// 已经在响的告警，保留最早的开始时间，只刷新当前值：这样页面上看到的是
	// 「这条告警从几点开始响、现在偏离到多少」，而不是每秒都被重置成新告警。
	since := at
	if s.active {
		since = s.since
	}

	alert := Alert{
		Rule:      rule.Name,
		Metric:    rule.Metric,
		Direction: rule.Direction,
		Value:     value,
		Baseline:  baseline,
		Sigma:     sigma,
		Threshold: threshold,
		Peak:      value,
		Since:     since,
		Reason:    reason,
	}

	return alert, true
}

// push 把一个正常值放进基线窗口，超出窗口长度就把最老的挤出去。
func (s *ruleState) push(value float64) {
	s.values = append(s.values, value)
	if len(s.values) > s.rule.Window {
		s.values = s.values[len(s.values)-s.rule.Window:]
	}
}

// record 记一次状态变化，超过上限就丢掉最老的。
func (e *Engine) record(event AlertEvent) {
	e.events = append(e.events, event)
	if len(e.events) > e.limit {
		e.events = e.events[len(e.events)-e.limit:]
	}
}

// Active 返回此刻正在响的告警。
func (e *Engine) Active() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	alerts := make([]Alert, 0, len(e.states))

	for _, state := range e.states {
		if !state.active {
			continue
		}

		alert := state.last
		alert.Peak = state.peak
		alert.ElapsedSeconds = now.Sub(state.since).Seconds()
		alerts = append(alerts, alert)
	}

	return alerts
}

// Events 返回最近的告警状态变化（含已经恢复的），最新的排在最前面。
func (e *Engine) Events() []AlertEvent {
	e.mu.Lock()
	defer e.mu.Unlock()

	events := make([]AlertEvent, 0, len(e.events))
	for i := len(e.events) - 1; i >= 0; i-- {
		events = append(events, e.events[i])
	}

	return events
}

// Status 返回每条规则此刻的体检数据：基线、波动、攒了多少样本、有没有在响。
func (e *Engine) Status() []RuleStatus {
	e.mu.Lock()
	defer e.mu.Unlock()

	statuses := make([]RuleStatus, 0, len(e.states))
	for _, state := range e.states {
		baseline, sigma := meanStdDev(state.values)
		statuses = append(statuses, RuleStatus{
			Name:        state.rule.Name,
			Metric:      state.rule.Metric,
			Direction:   state.rule.Direction,
			Floor:       state.rule.Floor,
			Sigma:       state.rule.Sigma,
			Consecutive: state.rule.Consecutive,
			Baseline:    baseline,
			StdDev:      sigma,
			Samples:     len(state.values),
			Streak:      state.streak,
			Active:      state.active,
			Current:     state.current,
			HasCurrent:  state.hasCurrent,
		})
	}

	return statuses
}

// meanStdDev 算出一组数的平均值和标准差（总体标准差，除以 n 而不是 n-1）。
//
// 标准差在这里的含义就是「平时波动有多大」：基线 100、波动 2，那 120 就是异常；
// 基线 100、波动 50（流量本来就忽高忽低），120 只能算正常范围。
func meanStdDev(values []float64) (mean, stddev float64) {
	if len(values) == 0 {
		return 0, 0
	}

	var sum float64
	for _, value := range values {
		sum += value
	}
	mean = sum / float64(len(values))

	var squares float64
	for _, value := range values {
		diff := value - mean
		squares += diff * diff
	}

	return mean, math.Sqrt(squares / float64(len(values)))
}
