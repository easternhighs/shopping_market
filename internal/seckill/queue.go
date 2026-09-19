package seckill

import (
	"context"
	"encoding/json"
	"sync/atomic"

	kafkaclient "shopping_market/internal/pkg/kafka"
)

// OrderMessage 是发送到 Kafka 的秒杀订单消息。
type OrderMessage struct {
	RequestID  string `json:"request_id"`
	ActivityID uint   `json:"activity_id"`
	ItemID     uint   `json:"item_id"`
	SKUID      uint   `json:"sku_id"`
	UserID     uint   `json:"user_id"`
	Quantity   int    `json:"quantity"`
}

// ConsumedOrder 是“一条已经从 Kafka 读出来、并且解析过的秒杀订单消息”。
//
//   - Raw：Kafka 原始消息。这一批处理完后要原样交给 Commit，才能提交消费进度。
//   - Order：解析出来的订单内容。
//   - Err：解析失败时非 nil。这种“坏消息”不能悄悄丢掉（丢掉会让消费进度卡住），
//     正确做法是：跳过它的业务处理，但仍然把它算进这一批一起提交，让它翻篇。
type ConsumedOrder struct {
	Raw   kafkaclient.Message
	Order OrderMessage
	Err   error
}

// Queue 封装秒杀模块使用的 Kafka 生产者和消费者。
type Queue struct {
	producer  *kafkaclient.Producer
	consumers []*kafkaclient.Consumer

	// pending 记录已经发进 Kafka、但消费端还没提交落库的订单条数。
	// 它回答一个很具体的问题：此刻有没有订单正在途中（Redis 已扣、MySQL 还没扣）。
	pending atomic.Int64

	// idle 记录每个消费者最近一次拉取是不是空的。
	// 全为 true 说明每个分区都被追平过，队列里没有历史积压。
	idle map[*kafkaclient.Consumer]*atomic.Bool
}

// NewQueue 创建秒杀 Kafka 队列。
//
// 传入多个 consumer（同一个消费组）就能让多个 goroutine 并发消费；
// 但数量不要超过 topic 的分区数，多出来的消费者会分不到分区、白白空转。
func NewQueue(producer *kafkaclient.Producer, consumers ...*kafkaclient.Consumer) *Queue {
	idle := make(map[*kafkaclient.Consumer]*atomic.Bool, len(consumers))
	for _, consumer := range consumers {
		idle[consumer] = new(atomic.Bool)
	}

	return &Queue{producer: producer, consumers: consumers, idle: idle}
}

// Consumers 返回队列里的所有消费者，消费端会给每个消费者启动一个 goroutine。
func (q *Queue) Consumers() []*kafkaclient.Consumer {
	return q.consumers
}

// Publish 把秒杀订单消息发送到 Kafka。
func (q *Queue) Publish(ctx context.Context, message OrderMessage) error {
	byteMessage, err := json.Marshal(message)
	if err != nil {
		return err
	}

	err = q.producer.Write(ctx, byteMessage)
	if err != nil {
		return err
	}

	// 消息确实进了 Kafka 才记账：从现在起到消费端提交为止，这条订单都算在途。
	q.pending.Add(1)

	return nil
}

// ConsumeBatch 用指定的消费者批量读取并解析秒杀订单消息。
//
// 取到原始消息后逐条 json.Unmarshal 成 OrderMessage：
//   - 成功：把解析结果填进 Order；
//   - 失败：把错误填进 Err。这一条不做业务处理，但仍然跟着这一批一起提交，
//     这样一条坏消息不会把整个队列卡死。
//
// 注意：真错误（网络、权限等）由 FetchBatch 直接返回，这里原样往上抛。
// 另外每条原始消息都要包成 ConsumedOrder 返回，一条都不能少，
// 否则这一批提交的消费进度就会漏掉它。
func (q *Queue) ConsumeBatch(ctx context.Context, consumer *kafkaclient.Consumer, maxBatch int) ([]ConsumedOrder, error) {
	kMsgs, err := consumer.FetchBatch(ctx, maxBatch)
	if err != nil {
		return nil, err
	}

	// 记下这个消费者这一轮是不是拉空了。拉空说明它负责的分区已经追平，
	// 对账任务要等所有消费者都追平过，才敢动 Redis 里的库存。
	if flag := q.idle[consumer]; flag != nil {
		flag.Store(len(kMsgs) == 0)
	}

	var consumedOrders []ConsumedOrder
	for _, kMsg := range kMsgs {
		var msg OrderMessage
		err := json.Unmarshal(kMsg.Value, &msg)

		consumedOrder := ConsumedOrder{
			Raw:   kMsg,
			Order: msg,
		}

		if err != nil {
			consumedOrder.Err = err
		}

		consumedOrders = append(consumedOrders, consumedOrder)
	}

	return consumedOrders, nil
}

// Commit 提交一批消息的消费进度。
//
// 关键点：必须“业务处理完”再提交。先提交后处理的话，一旦进程崩溃，消息就真的丢了。
func (q *Queue) Commit(ctx context.Context, consumer *kafkaclient.Consumer, batch []ConsumedOrder) error {
	// 判断“这一批是不是空的”要用 len(batch) == 0，不要写 batch == nil：
	// 空切片（长度 0 但不是 nil）也该直接返回，否则会白白空提交一次。
	if len(batch) == 0 {
		return nil
	}

	var kMsgs []kafkaclient.Message

	for _, v := range batch {
		kMsgs = append(kMsgs, v.Raw)
	}

	if err := consumer.Commit(ctx, kMsgs); err != nil {
		return err
	}

	// 这一批（含解析失败、以及落库失败后已经回补过 Redis 的消息）都翻篇了，
	// 在途计数减回去。
	q.pending.Add(-int64(len(batch)))

	return nil
}

// Pending 报告此刻有多少订单「已经发进 Kafka、但消费端还没提交落库」。
//
// 它有两个用处：
//   - 对账任务用它判断「现在能不能安全地覆盖 Redis 库存」（见 Settled）；
//   - 演示控制台用它当作「Kafka 堆积」的近似值，实时显示有多少订单还在路上。
func (q *Queue) Pending() int64 {
	return q.pending.Load()
}

// Settled 报告此刻队列是不是真的安静下来了：没有任何订单在途中，
// 并且每个消费者都至少追平过一次。
//
// 为什么对账任务必须先问这一句：对账是拿 MySQL 的 sold 反推 Redis 剩余库存
// （剩余 = 总库存 - 已售）。如果此刻还有订单在途中（Redis 已经扣了、MySQL 还没记账），
// sold 就是偏小的，反推出来的剩余库存就是偏大的。
// 把这个偏大的数字写回 Redis，等于凭空多出一批库存，
// 高并发下会让本该被拒绝的请求通过，也就是超卖。
//
// 为什么还要看 idle：进程刚重启时 pending 是 0（这个进程还没发过消息），
// 但 Kafka 里可能还压着上一轮没消费完的消息。只有等到每个消费者都拉空过一次，
// 才能确认历史积压真的清完了。
func (q *Queue) Settled() bool {
	if q.pending.Load() != 0 {
		return false
	}

	for _, flag := range q.idle {
		if !flag.Load() {
			return false
		}
	}

	return true
}
