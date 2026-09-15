package kafka

import (
	"context"

	"github.com/segmentio/kafka-go"
)

// Producer 是对 kafka-go Writer 的轻量封装。
type Producer struct {
	writer *kafka.Writer
}

// NewProducer 创建 Kafka 生产者。
func NewProducer(brokers []string, topic string) *Producer {
	writer := &kafka.Writer{
		Addr:  kafka.TCP(brokers...),
		Topic: topic,
	}
	return &Producer{writer: writer}
}

// Write 向 Kafka 发送一条消息。
func (p *Producer) Write(ctx context.Context, value []byte) error {
	return p.writer.WriteMessages(ctx, kafka.Message{Value: value})
}

// Consumer 是对 kafka-go Reader 的轻量封装。
type Consumer struct {
	reader *kafka.Reader
}

// NewConsumer 创建 Kafka 消费者。
func NewConsumer(brokers []string, topic, groupID string) *Consumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: groupID,
	})
	return &Consumer{reader: reader}
}

// Read 从 Kafka 读取一条消息。
func (c *Consumer) Read(ctx context.Context) ([]byte, error) {
	message, err := c.reader.ReadMessage(ctx)
	if err != nil {
		return nil, err
	}
	return message.Value, nil
}
