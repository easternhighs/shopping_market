package kafka

import (
	"context"
	"errors"
	"time"

	"github.com/segmentio/kafka-go"
)

// Message 是队列里的一条消息。
//
// Value 是消息内容（本项目里是一段 JSON）。
// raw 是 kafka-go 的原始消息，它额外记着“这条消息来自哪个分区、是第几个位置”，
// 提交消费进度（offset）时必须把它原样还给 Kafka，所以要留着它。
// raw 是小写开头，包外只能传递、不能自己伪造：只能由 FetchBatch 产生，再交给 Commit。
type Message struct {
	Value []byte
	raw   kafka.Message
}

// Producer 是对 kafka-go Writer 的轻量封装。
type Producer struct {
	writer *kafka.Writer
}

// NewProducer 创建 Kafka 生产者。
//
// 秒杀入口会不停地发消息。如果每发一条都停下来等 Kafka 确认一次，
// 光网络往返就能把接口拖慢，所以让它“攒一小批再一起发”：
//   - BatchSize：攒够 100 条就发；
//   - BatchTimeout：没攒够也不能一直等，最多 10 毫秒就发出去。
func NewProducer(brokers []string, topic string) *Producer {
	writer := &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        topic,
		BatchSize:    100,
		BatchTimeout: 10 * time.Millisecond,
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
//
// 一个重要事实：同一个消费组（groupID）下每创建一个 Consumer，
// Kafka 就把它当成“一个独立的消费者”，把 topic 的分区分一部分给它。
// 所以想并发消费就得创建多个 Consumer；反过来，消费者比分区多时，
// 多出来的消费者分不到分区，只能空等。
func NewConsumer(brokers []string, topic, groupID string) *Consumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: groupID,

		// 一次拉取请求的“最少拿多少 / 最多拿多少 / 最多等多久”。
		MinBytes: 1,        // 哪怕只有 1 字节也马上返回，别空等
		MaxBytes: 10 << 20, // 最多 10MB，防止一批太大撑爆内存
		MaxWait:  100 * time.Millisecond,

		// 本地缓冲：网络抖动时先把拉到的消息暂存在这里，消费端慢一点也不至于立刻卡死。
		QueueCapacity: 1000,

		// CommitInterval=0 表示“关掉后台自动提交”，改成我们自己手动提交：
		// 一批消息全部处理完，才把这批的消费进度一次性告诉 Kafka。
		// 这样既保留了“落库成功才提交”的可靠性，又把提交次数从“每条一次”
		// 降到“每批一次”。
		//
		// 这里要特别小心：kafka-go 的 ReadMessage 在消费组模式下会“读一条就顺手同步提交一次”，
		// 那是原来消费慢的主要原因之一。FetchMessage 不会自动提交，这正是我们要的。
		CommitInterval: 0,
	})
	return &Consumer{reader: reader}
}

// FetchBatch 批量拉取消息：一次最多拉 maxBatch 条，拉不满就等一小会儿返回已有的。
//
// 为什么要批量：原来用 ReadMessage 一次拿一条，而且它每拿一条就同步提交一次消费进度，
// 相当于一条消息两次网络往返。在 Windows 的 Docker Desktop 上这种往返特别慢，
// 实测消费速度只有几百条/秒。改成批量后，取一次拿一批、提交一次记一批，
// 网络往返次数直接除以批量大小。
//
// 返回值说明：
//   - msgs：这一批消息，可能少于 maxBatch，也可能是空的；
//   - err：真正出错才返回错误；“这次没凑满 / 暂时没有新消息”不算错误——
//     内部给这一批设了 200 毫秒的上限，到点就把已经拿到的消息正常返回。
//
// 千万注意：FetchMessage 只负责“读到”，不会自动提交消费进度。
// 业务处理成功之后，调用方必须再调用 Commit，否则重启后这些消息会被重复消费。
func (c *Consumer) FetchBatch(ctx context.Context, maxBatch int) ([]Message, error) {
	batchCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	var msgs []Message

Loop:
	for range maxBatch {
		select {
		case <-batchCtx.Done():
			break Loop
		default:
		}

		msg, err := c.reader.FetchMessage(batchCtx)
		if err != nil {
			if errors.Is(err, batchCtx.Err()) {
				break Loop
			}
			return nil, err
		}

		msgs = append(msgs, Message{Value: msg.Value, raw: msg})
	}

	return msgs, nil
}

// Commit 把这一批消息的消费进度（offset）一次性提交给 Kafka。
//
// 提交之后 Kafka 就认为“这批消息消费完了”，进程崩溃重启也会从没提交的位置继续。
// 所以必须等业务真正处理完再调用它。
func (c *Consumer) Commit(ctx context.Context, msgs []Message) error {
	// 用 len 判断而不是和 nil 比较：空切片、非 nil 的空切片都该直接返回。
	if len(msgs) == 0 {
		return nil
	}

	var msgBatch []kafka.Message
	for _, msg := range msgs {
		msgBatch = append(msgBatch, msg.raw)
	}

	if err := c.reader.CommitMessages(ctx, msgBatch...); err != nil {
		return err
	}

	return nil
}

// Close 关闭消费者，释放它占用的连接。
func (c *Consumer) Close() error {
	return c.reader.Close()
}
