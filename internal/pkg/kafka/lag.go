package kafka

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// LagProbe 负责回答一个问题：**消费组现在落后多少条消息还没处理**（也就是 LAG）。
//
// 为什么要专门做这么一个探针：LAG 是「削峰有没有削住」最直接的证据。
// 订单先写进 Kafka，再由消费端慢慢落库；只要生产速度长期快过消费速度，
// LAG 就会一路涨。这时候 Redis 已经扣了库存、MySQL 还没记账，
// 系统的账目处于「中间态」。能让系统自己盯着这个数字，就不用靠人盯屏幕。
//
// 为什么不在采样的时候现查：查一次 LAG 要跟 Kafka 来回三次网络请求，
// 放在采样循环里会拖慢采样节奏。所以这里让它在自己的 goroutine 里定时刷新，
// 采样时只是读一下内存里的缓存值（Lag 方法），代价接近零。
//
// 怎么算出来的（和 kafka-consumer-groups 工具是同一套账）：
//
//	LAG = 每个分区的「最后一条消息的位置」 − 每个分区的「消费组已提交的位置」
//
// 前者问 broker 要（ListOffsets），后者问消费组协调者要（OffsetFetch）。
// 两者相减再按分区求和，就是整个 topic 积压了多少条。
type LagProbe struct {
	client   *kafka.Client
	topic    string
	groupID  string
	interval time.Duration
	logf     func(format string, args ...any)

	mu       sync.RWMutex
	lag      int64
	ready    bool
	failures int
}

// NewLagProbe 创建 LAG 探针。brokers 就是 Kafka 地址，interval 是刷新间隔（本项目 5 秒）。
func NewLagProbe(brokers []string, topic, groupID string, interval time.Duration) *LagProbe {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	return &LagProbe{
		client: &kafka.Client{
			Addr:    kafka.TCP(brokers...),
			Timeout: 5 * time.Second,
		},
		topic:    topic,
		groupID:  groupID,
		interval: interval,
		logf:     log.Printf,
	}
}

// Start 启动后台刷新，直到 ctx 结束。
func (p *LagProbe) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		p.refresh(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx)
			}
		}
	}()
}

// Lag 返回最近一次采集到的 LAG，是纯内存读取，不会阻塞调用方。
//
// 返回值约定：还没成功采集过时返回 -1。之所以不用 0 表示「不知道」，
// 是因为 0 的含义是「一条都没积压」，把「探针没跑起来」当成「一切正常」，
// 会把真正的积压问题悄悄盖掉。
func (p *LagProbe) Lag() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.ready {
		return -1
	}
	return p.lag
}

// refresh 采集一次并写进缓存。
func (p *LagProbe) refresh(ctx context.Context) {
	lag, err := p.query(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()

	if err != nil {
		p.failures++
		// 连续失败才打日志：Kafka 抖动一下不值得刷屏，但一直失败必须留痕。
		if p.failures == 1 || p.failures%12 == 0 {
			p.logf("[LAG 探针] 采集失败（第 %d 次）：%v", p.failures, err)
		}
		// 采集失败时保留上一次成功的值：把「探针坏了」当成「LAG 归零」，
		// 等于在最需要告警的时候把告警关掉。
		return
	}

	if p.failures > 0 {
		p.logf("[LAG 探针] 已恢复正常（此前连续失败 %d 次）", p.failures)
		p.failures = 0
	}

	p.lag = lag
	p.ready = true
}

// query 真正去问 Kafka：先拿分区列表，再分别要高水位和已提交位点。
func (p *LagProbe) query(ctx context.Context) (int64, error) {
	metadata, err := p.client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{p.topic}})
	if err != nil {
		return 0, fmt.Errorf("读取 topic 元数据: %w", err)
	}

	var (
		partitions   []int
		lastRequests []kafka.OffsetRequest
	)

	for _, topic := range metadata.Topics {
		if topic.Name != p.topic {
			continue
		}
		if topic.Error != nil {
			return 0, fmt.Errorf("topic %s 元数据异常: %w", p.topic, topic.Error)
		}

		for _, partition := range topic.Partitions {
			if partition.Error != nil {
				continue
			}
			partitions = append(partitions, partition.ID)
			lastRequests = append(lastRequests, kafka.LastOffsetOf(partition.ID))
		}
	}

	if len(partitions) == 0 {
		return 0, fmt.Errorf("topic %s 没有可用分区（topic 还没建？）", p.topic)
	}

	sort.Ints(partitions)

	// 高水位：每个分区一共写进来多少条消息（Kafka 里的「位置」是下一条消息的下标）。
	high, err := p.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{p.topic: lastRequests},
	})
	if err != nil {
		return 0, fmt.Errorf("读取分区高水位: %w", err)
	}

	highByPartition := make(map[int]int64, len(partitions))
	for _, offsets := range high.Topics[p.topic] {
		if offsets.Error != nil {
			continue
		}
		highByPartition[offsets.Partition] = offsets.LastOffset
	}

	// 已提交位点：消费组确认「已经处理完」的位置。OffsetFetch 由 kafka-go 自动
	// 找到消费组协调者（broker 上的一个角色），不需要我们手动指路。
	committed, err := p.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: p.groupID,
		Topics:  map[string][]int{p.topic: partitions},
	})
	if err != nil {
		return 0, fmt.Errorf("读取消费组已提交位点: %w", err)
	}

	var total int64

	for _, offset := range committed.Topics[p.topic] {
		if offset.Error != nil {
			continue
		}

		highWater := highByPartition[offset.Partition]

		// CommittedOffset 为 -1 表示这个消费组在该分区还没提交过任何位点
		// （全新的消费组）。这时按 0 算：也就是「从第一条开始都还没消费」。
		done := offset.CommittedOffset
		if done < 0 {
			done = 0
		}

		lags := highWater - done
		if lags > 0 {
			total += lags
		}
	}

	return total, nil
}
