package seckill

import (
	"context"
	"encoding/json"

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
}

// NewQueue 创建秒杀 Kafka 队列。
//
// 传入多个 consumer（同一个消费组）就能让多个 goroutine 并发消费；
// 但数量不要超过 topic 的分区数，多出来的消费者会分不到分区、白白空转。
func NewQueue(producer *kafkaclient.Producer, consumers ...*kafkaclient.Consumer) *Queue {
	return &Queue{producer: producer, consumers: consumers}
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

	return nil
}
