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

// Queue 封装秒杀模块使用的 Kafka 生产者和消费者。
type Queue struct {
	producer *kafkaclient.Producer
	consumer *kafkaclient.Consumer
}

// NewQueue 创建秒杀 Kafka 队列。
func NewQueue(producer *kafkaclient.Producer, consumer *kafkaclient.Consumer) *Queue {
	return &Queue{producer: producer, consumer: consumer}
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

// Consume 从 Kafka 读取一条秒杀订单消息。
func (q *Queue) Consume(ctx context.Context) (OrderMessage, error) {
	byteMsg, err := q.consumer.Read(ctx)
	if err != nil {
		//Kafka不能传null空值，否则会作为tombmessage将对应项删除
		return OrderMessage{}, err
	}

	var msg OrderMessage
	err = json.Unmarshal(byteMsg, &msg)
	if err != nil {
		return OrderMessage{}, err
	}

	return msg, nil
}
