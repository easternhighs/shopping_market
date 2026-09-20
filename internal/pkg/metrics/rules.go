package metrics

import "fmt"

// DefaultRules 返回本项目默认开启的告警规则。
//
// 规则不多，四条，覆盖四种「真出事」的样子：
//  1. 突发流量——秒杀场景的常态，也是压测时最该被注意到的事；
//  2. 服务端错误率——真的坏了（不是限流那种正常拒绝）；
//  3. Kafka 消费积压——削峰的那一段堵住了；
//  4. 在途订单积压——同一条队列的另一个视角，本进程自己的账。
//
// 为什么没有「流量骤降」这条规则：它是唯一一条 below 方向的规则，
// 而我们没有默认打开它。原因是压测一结束流量必然骤降，这是**已知的正常回落**，
// 打开了每次压测完都会误报一次，把真正的异常淹掉。要判断「流量掉了是不是故障」，
// 得结合「同时错误率有没有升高、是不是有人手动停的」一起看，属于相关性判断。
// 引擎支持 below 方向（见 DirectionBelow），需要时可以自己加进来。
func DefaultRules() []Rule {
	return []Rule{
		{
			Name:        "突发流量",
			Metric:      MetricQPS,
			Direction:   DirectionAbove,
			Floor:       500,
			Sigma:       4,
			Window:      30,
			MinSamples:  10,
			Consecutive: 3,
			Reason: func(cur, baseline, sigma, threshold float64) string {
				return fmt.Sprintf("QPS 冲到 %.0f/s，平时约 %.1f/s（波动 ±%.1f），越过门槛 %.0f/s，已连续几个采样窗口偏高。这是流量突然打进来了：先看限流是否生效、Redis 扣减是否正常、Kafka 有没有开始积压。",
					cur, baseline, sigma, threshold)
			},
		},
		{
			Name:        "服务端错误率",
			Metric:      MetricErrorRate,
			Direction:   DirectionAbove,
			Floor:       1,
			Sigma:       3,
			Window:      30,
			MinSamples:  10,
			Consecutive: 2,
			Reason: func(cur, baseline, sigma, threshold float64) string {
				return fmt.Sprintf("5xx 错误率 %.2f%%，平时 %.2f%%（波动 ±%.2f，门槛 %.2f%%）。有请求真的失败了——这不是限流那种正常拒绝，建议立刻去日志里找 panic、数据库连接和 Redis 报错。",
					cur, baseline, sigma, threshold)
			},
		},
		{
			Name:        "Kafka 消费积压",
			Metric:      MetricKafkaLag,
			Direction:   DirectionAbove,
			Floor:       1000,
			Sigma:       4,
			Window:      30,
			MinSamples:  10,
			Consecutive: 3,
			Reason: func(cur, baseline, sigma, threshold float64) string {
				return fmt.Sprintf("消费组积压 %.0f 条消息，平时 %.0f 条（门槛 %.0f 条）。订单已经发进 Kafka 但还没落库：这段时间 Redis 已经扣了库存、MySQL 还没记账。先确认消费者还活着、分区数够不够。",
					cur, baseline, threshold)
			},
		},
		{
			Name:        "在途订单积压",
			Metric:      MetricPending,
			Direction:   DirectionAbove,
			Floor:       1000,
			Sigma:       4,
			Window:      30,
			MinSamples:  10,
			Consecutive: 3,
			Reason: func(cur, baseline, sigma, threshold float64) string {
				return fmt.Sprintf("在途订单 %.0f 条，平时 %.0f 条（门槛 %.0f 条）。这些订单 Redis 已经扣了库存、MySQL 还没记账，消费端一旦长时间追不上，就会出现「页面显示已抢到、实际还没落库」的窗口期。",
					cur, baseline, threshold)
			},
		},
	}
}
