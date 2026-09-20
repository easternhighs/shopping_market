package ratelimit

import (
	"context"
	"log"
	"sync"
	"time"
)

// Controller 决定「此刻每秒最多放进来多少个请求」。
//
// 思路借用了网络里的 AIMD（Additive Increase / Multiplicative Decrease）：
//   - 系统健康（延迟低、没错误）→ 每个窗口加一点（加性增），慢慢试探上限；
//   - 系统吃力（延迟超标或开始报错）→ 立刻砍掉一截（乘性减），先把压力降下来。
//
// 为什么是「慢慢加、快速减」：加得太快等于自己制造雪崩，减得太慢等于看着系统被打死。
// 这和 TCP 拥塞控制是同一个道理，也就是「自适应」两个字的全部含义。
//
// 判据用的是**平均延迟**和**错误率**，不是「请求多不多」：
// 请求多本身不是坏事，请求多到处理不过来（延迟涨上去）才是。
type Controller struct {
	mu       sync.Mutex
	limit    int64
	minLimit int64
	maxLimit int64
	step     int64
	target   time.Duration
	cooldown int
	// lowest 记「上一次顶到上限之后，最低被砍到过多少」，
	// 只用来在日志里说清楚「从多惨恢复回来的」。不记这个数的话，
	// 报出来的会是「刚回上限前的那个值」，看着跟真事不符。
	lowest int64
	logf   func(format string, args ...any)
}

// errorRateCeiling 是「算什么都没错」的上限：错误率超过它就不再放量，只能收紧。
const errorRateCeiling = 1.0

// increaseCooldownWindows 是砍完之后要等几个窗口才重新尝试加量。
// 没有这个冷却时间的话，一砍一加会来回抖，阈值永远在门槛附近抽搐。
const increaseCooldownWindows = 2

// NewController 创建自适应控制器。
//
//   - minLimit：再惨也不低于这个值（留一条活路，别把管理接口彻底关死）；
//   - maxLimit：静态上限，再空闲也不会超过它（这一层保证「自适应」不可能放大到失控）；
//   - target：目标平均延迟，超过它就算「系统开始吃力」。
func NewController(minLimit, maxLimit int64, target time.Duration) *Controller {
	if minLimit <= 0 {
		minLimit = 1
	}
	if maxLimit < minLimit {
		maxLimit = minLimit
	}

	return &Controller{
		limit:    maxLimit,
		minLimit: minLimit,
		maxLimit: maxLimit,
		step:     maxInt64(1, maxLimit/20),
		target:   target,
		lowest:   maxLimit,
		logf:     log.Printf,
	}
}

// Current 返回此刻生效的阈值。
func (c *Controller) Current() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

// Target 返回目标延迟，报告和演示页要用。
func (c *Controller) Target() time.Duration {
	return c.target
}

// Observe 吃一个采样窗口的数据，决定下一窗口的阈值。
//
// 参数来自采样器：avgLatencyMS 是这一窗口的平均延迟（毫秒），
// errorRatePercent 是 5xx 错误率（百分数）。
func (c *Controller) Observe(avgLatencyMS, errorRatePercent float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	latency := time.Duration(avgLatencyMS * float64(time.Millisecond))
	healthy := latency <= c.target && errorRatePercent < errorRateCeiling

	if !healthy {
		next := c.limit * 7 / 10 // 乘性减：一次砍掉 30%
		if next < c.minLimit {
			next = c.minLimit
		}

		if next != c.limit {
			c.logf("[自适应限流] 平均延迟 %.0fms（目标 %s），错误率 %.2f%%：阈值 %d → %d 次/秒",
				avgLatencyMS, c.target, errorRatePercent, c.limit, next)
			c.limit = next
			if next < c.lowest {
				c.lowest = next
			}
		}

		c.cooldown = increaseCooldownWindows
		return
	}

	if c.cooldown > 0 {
		c.cooldown--
		return
	}

	next := c.limit + c.step
	if next > c.maxLimit {
		next = c.maxLimit
	}

	if next != c.limit {
		c.limit = next
		if next == c.maxLimit {
			c.logf("[自适应限流] 系统已恢复，阈值回到上限 %d 次/秒（此前最低降到 %d）", c.maxLimit, c.lowest)
			c.lowest = c.maxLimit
		}
	}
}

// AdaptiveLimiter 在原来那个固定窗口限流器外面，套一层「动态阈值」。
//
// 它和原限流器的分工是这样的：
//   - RedisLimiter 负责「数数」：某个 key 在这个窗口里第几次请求，超了就不让过；
//   - Controller 负责「定标准」：这一秒最多放几个。
//
// 为什么不直接改 RedisLimiter：限流在抢购热路径里是和扣库存共用一次 Redis 往返的
// （见 internal/seckill/cache.go 的 Lua 脚本），那里动一下就是拿 QPS 换功能。
// 这一层只在管理类接口上用，属于旁路，风险低、效果看得见。
type AdaptiveLimiter struct {
	base Limiter
	ctrl *Controller
}

// NewAdaptiveLimiter 把动态阈值套在任意 Limiter 上。
func NewAdaptiveLimiter(base Limiter, ctrl *Controller) *AdaptiveLimiter {
	return &AdaptiveLimiter{base: base, ctrl: ctrl}
}

// Allow 用「动态阈值」而不是调用方给的固定值来判断。
//
// 关键约束：动态值只能比调用方给的静态上限更小或相等，永远不可能更大。
// 这样即使控制器算错了、或者被外部输入带偏，也不可能把闸门放大到失控。
func (a *AdaptiveLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	effective := a.ctrl.Current()
	if effective <= 0 || effective > int64(limit) {
		effective = int64(limit)
	}

	return a.base.Allow(ctx, key, int(effective), window)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
