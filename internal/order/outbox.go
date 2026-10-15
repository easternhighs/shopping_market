package order

import (
	"encoding/json"
	"fmt"
	"time"
)

// 补偿（重试）策略相关的常量。
//
// 为什么要退避（backoff）：库存服务如果正忙，我们每 2 秒去敲一次门，只是给它添堵。
// 改成「第一次等 1 秒、第二次 2 秒、第三次 4 秒……最多等 60 秒」，
// 对方缓过来的时候我们正好下一次敲门。这个套路叫「指数退避」。
const (
	// RetryInterval 是后台补偿任务隔多久扫一遍「待办便签」。
	RetryInterval = 2 * time.Second
	// RetryBatchSize 是一轮最多处理几条：抓太多会把内存和对方一起拖垮。
	RetryBatchSize = 20
	// OutboxMaxAttempts 是重试次数上限，用光就放弃并关单。
	OutboxMaxAttempts = 8
	// OutboxLease 是「领取租约」的长度：领走之后这段时间内，别的进程不许再动它。
	OutboxLease = 30 * time.Second
	// retryMaxDelay 是退避时间的上限。
	retryMaxDelay = 60 * time.Second
)

// OutboxItem 是写进 order_outbox.payload 里的一个明细。
//
// 它和 PlaceItem 内容一样、但特意分开：PlaceItem 是「接口入参」，
// OutboxItem 是「存进数据库的消息体」。以后接口改了字段名，
// 已经落库的老消息还得按老格式读出来，两者混用就会把老消息读坏。
type OutboxItem struct {
	SKUID    uint `json:"sku_id"`
	Quantity int  `json:"quantity"`
}

// marshalOutboxPayload 把下单明细序列化成 JSON 字符串，准备存进 payload 列。
func marshalOutboxPayload(items []PlaceItem) (string, error) {
	out := make([]OutboxItem, 0, len(items))
	for _, item := range items {
		out = append(out, OutboxItem{SKUID: item.SKUID, Quantity: item.Quantity})
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("序列化 outbox 消息失败: %w", err)
	}
	return string(raw), nil
}

// unmarshalOutboxPayload 把 payload 还原成明细，供重试时使用。
func unmarshalOutboxPayload(payload string) ([]OutboxItem, error) {
	var items []OutboxItem
	if err := json.Unmarshal([]byte(payload), &items); err != nil {
		return nil, fmt.Errorf("解析 outbox 消息失败: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("outbox 消息里没有明细")
	}
	return items, nil
}

// toDeductItems 把「下单入参」转成「跨服务扣库存的入参」。
func toDeductItems(items []PlaceItem) []DeductItem {
	out := make([]DeductItem, 0, len(items))
	for _, item := range items {
		out = append(out, DeductItem{SKUID: item.SKUID, Quantity: item.Quantity})
	}
	return out
}

// fromOutbox 把「便签里存的明细」转成「跨服务扣库存的入参」。
func fromOutbox(items []OutboxItem) []DeductItem {
	out := make([]DeductItem, 0, len(items))
	for _, item := range items {
		out = append(out, DeductItem{SKUID: item.SKUID, Quantity: item.Quantity})
	}
	return out
}

// nextRetryDelay 算出「第 attempts 次失败之后，隔多久再试」。
//
// 规律是 1s、2s、4s、8s、16s、32s、60s（封顶 60 秒）。
// 传入的 attempts 是「已经失败了几次」，小于 1 按 1 算。
//
// 两个容易踩的坑：
//  1. 不能直接写 time.Second << attempts：次数一多就会整数溢出，
//     算出来的等待时间变成负数或乱值，重试节奏直接崩掉；
//  2. 必须封顶：不封顶的话第 30 次就要等 34 年，等于永远不再重试。
func nextRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}

	// 先夹住移位位数，再算时间：1<<6 = 64 秒，已经超过上限，再大就没意义了。
	shift := attempts - 1
	if shift > 6 {
		shift = 6
	}

	delay := time.Second * time.Duration(int64(1)<<uint(shift))
	if delay > retryMaxDelay || delay <= 0 {
		return retryMaxDelay
	}
	return delay
}
