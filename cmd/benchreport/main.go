// Command benchreport 把「一次压测留下的原始材料」整理成一份 Markdown 报告。
//
// 为什么需要它：压测的时候人会盯着 k6 的控制台看，但压测一结束，
// 「刚才这一轮到底发生了什么」就散落在三个地方了——k6 的 summary 文件、
// 服务进程的日志、服务端 /metrics 接口里的数字。等到写文档、准备面试、
// 或者回头查「上次为什么 p99 那么高」的时候，再一份份翻出来对照，非常费劲。
//
// 这个工具做的事很朴素：把这几份材料收集起来，按固定结构拼成一份报告，
// 再用几条写死的阈值指出「哪些数字看着不对劲」。
//
// 它刻意**不使用大模型**，原因有两个：
//  1. 判断「错误率是不是偏高」「LAG 是不是堆了」都有明确的数字标准，
//     写死规则更快、更稳定，也不会因为网络问题拿不到结论；
//  2. 面试时每条结论都能说清楚是从哪个数字、哪条规则来的，
//     而「模型说的」很难解释，也不适合当作工程判断的依据。
//
// 用法（在项目根目录执行）：
//
//	go run ./cmd/benchreport -k6 .tmp/k6_summary.json -log .tmp/srv_demo.log -out -
//
// 典型流程是先跑压测并导出 summary：
//
//	k6 run --summary-export .tmp/k6_summary.json -e MODE=seckill -e VUS=256 ...
//	go run ./cmd/benchreport -k6 .tmp/k6_summary.json -log .tmp/srv_demo.log
//
// 不加 -out 时报告写到 docs/reports/bench-<时间>.md；-out - 表示只打印不落盘。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shopping_market/internal/pkg/metrics"
)

// ---------- k6 那一侧 ----------

// k6Summary 是 k6 `--summary-export` 产出的 JSON 里我们关心的那部分。
//
// 为什么值写成 json.RawMessage：k6 不同版本的输出格式不一样。老版本会把统计值
// 多包一层 values（{"http_reqs":{"type":"counter","values":{"count":1}}}），
// 新版本是平铺的（{"http_reqs":{"count":1,"rate":10}}）。先用 RawMessage 原样收着，
// 取值时两种都试一遍（见 statFrom），免得换个 k6 版本整份报告就空了。
type k6Summary struct {
	Metrics map[string]json.RawMessage `json:"metrics"`
}

func loadK6Summary(path string) (*k6Summary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var summary k6Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("解析失败（是不是没有用 --summary-export 导出？）: %w", err)
	}
	return &summary, nil
}

// statFrom 从一个指标的定义里取出某个统计值，平铺和嵌套两种格式都认。
func statFrom(raw json.RawMessage, stat string) (float64, bool) {
	var flat map[string]float64
	if err := json.Unmarshal(raw, &flat); err == nil {
		if value, ok := flat[stat]; ok {
			return value, true
		}
	}

	// 老格式会多一层 values：整份对象用 map[string]float64 解不开（type 字段是字符串），
	// 会走到这里来。
	var nested struct {
		Values map[string]float64 `json:"values"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil {
		if value, ok := nested.Values[stat]; ok {
			return value, true
		}
	}

	return 0, false
}

// value 是取数的统一入口。没给 -k6、或者文件里压根没有这个指标时返回 false，
// 报告里就写「未取到」而不是写一个骗人的 0。
func (s *k6Summary) value(metric, stat string) (float64, bool) {
	if s == nil || s.Metrics == nil {
		return 0, false
	}
	raw, ok := s.Metrics[metric]
	if !ok {
		return 0, false
	}
	return statFrom(raw, stat)
}

// ---------- 服务端那一侧 ----------

// historyResponse 对应 GET /metrics/history。
type historyResponse struct {
	IntervalSeconds float64              `json:"interval_seconds"`
	SampleCount     int                  `json:"sample_count"`
	Samples         []metrics.Sample     `json:"samples"`
	ActiveAlerts    []metrics.Alert      `json:"active_alerts"`
	Events          []metrics.AlertEvent `json:"events"`
}

// alertsResponse 对应 GET /metrics/alerts。
type alertsResponse struct {
	Active []metrics.Alert      `json:"active"`
	Events []metrics.AlertEvent `json:"events"`
	Rules  []metrics.RuleStatus `json:"rules"`
}

// fetchJSON 拉一个 JSON 接口填进 out 里。
//
// 这里用泛型是为了省掉「为三个接口各写一遍 http.Get + Decode + 错误处理」的重复。
func fetchJSON[T any](client *http.Client, url string, out *T) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("返回状态码 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("返回的不是预期的 JSON: %w", err)
	}
	return nil
}

// ---------- 日志那一侧 ----------

// logMarkers 是「这行日志值得抄进报告」的标记词。
//
// 只挑这几种：告警（系统自己发现的问题）、panic（程序崩了）、补偿失败（订单没能落库）。
// 不把所有日志都塞进报告，是因为报告是给人看的，几十万行日志贴进去等于没贴。
var logMarkers = []string{"[告警", "panic", "补偿失败"}

const (
	// logChunkBytes 是每次往回读多大一块。分块读是为了内存恒定：
	// 不管日志是 5 MB 还是 5 GB，同一时刻内存里只有一块。
	logChunkBytes = 64 << 10
	// logScanLimit 是最多往回扫多少字节。找不到就认了，不能为了找一行日志把
	// 整个几百 MB 的文件扫一遍——报告工具自己不能变成压测的一部分。
	logScanLimit = 64 << 20
	// logHighlightLimit 是报告里最多抄多少行日志。
	logHighlightLimit = 20
)

// readLogHighlights 从日志文件的尾部往回找「值得抄进报告」的行，最多 limit 条，
// 返回时按时间先后排列。
//
// 为什么是从后往前扫，而不是「读最后 N MB 再过滤」：
// 服务开着访问日志的时候一个请求一行，压测 20 秒就能写出十几 MB
// （实测 10 万个请求约 9 MB）。如果只读尾巴那几 MB，真正重要的告警行
// （往往在压测刚开始时就打出来了）会被后面铺天盖地的访问日志冲掉，一条都读不到。
// 从后往前按块扫、一边扫一边收集，凑够条数立刻停，既找得到又不会把内存吃光。
func readLogHighlights(path string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = logHighlightLimit
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	var (
		offset   = info.Size()
		scanned  int64
		carry    string   // 被块边界切开的那半行，等读到更靠前的块再拼起来
		reversed []string // 从后往前收集，返回前要翻过来
	)

	for offset > 0 && len(reversed) < limit && scanned < logScanLimit {
		readSize := int64(logChunkBytes)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize
		scanned += readSize

		buf := make([]byte, readSize)
		if _, err := file.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		text := string(buf) + carry
		if offset > 0 {
			// 这一块的开头可能正好切在某一行的中间：把不完整的部分留到下一轮，
			// 和更靠前的那一块拼起来再判断，免得因为断行漏掉一行告警。
			if index := strings.IndexByte(text, '\n'); index >= 0 {
				carry = text[:index]
				text = text[index+1:]
			} else {
				carry = text
				text = ""
			}
		} else {
			carry = ""
		}

		lines := strings.Split(text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if !hasLogMarker(lines[i]) {
				continue
			}
			reversed = append(reversed, strings.TrimSpace(lines[i]))
			if len(reversed) >= limit {
				break
			}
		}
	}

	highlights := make([]string, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		highlights = append(highlights, reversed[i])
	}
	return highlights, nil
}

// hasLogMarker 判断一行日志是不是「值得抄进报告」的那种。
func hasLogMarker(line string) bool {
	for _, marker := range logMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// ---------- 数字格式化 ----------

// num 把数字格式化成带千位分隔的样子：17963 → 17,963。
// 报告里动辄几万几十万的数字，加上分隔符才一眼看得出量级。
func num(value float64) string {
	negative := value < 0
	if negative {
		value = -value
	}

	text := strconv.FormatFloat(value, 'f', -1, 64)
	intPart, fracPart, hasFrac := strings.Cut(text, ".")

	var out strings.Builder
	for i := 0; i < len(intPart); i++ {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteByte(intPart[i])
	}

	result := out.String()
	if hasFrac {
		result += "." + fracPart
	}
	if negative {
		return "-" + result
	}
	return result
}

// f2 保留两位小数，用于延迟、百分比这类不需要千位分隔的数字。
func f2(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func orNone(text string) string {
	if strings.TrimSpace(text) == "" {
		return "（未提供）"
	}
	return text
}

// ---------- 从采样历史里取极值 ----------

const (
	modePeak   = "peak"
	modeAvg    = "avg"
	modeLowest = "lowest"
)

// pick 从采样历史里按 key 取一个数：
//   - peak：这段压测里的最高值（比如 QPS 峰值、LAG 峰值）；
//   - lowest：非零的最低值（比如自适应限流的最低阈值）；
//   - avg：整段的平均值。
//
// lowest 为什么要跳过 0：kafka_lag 这类指标在探针还没拿到数据时会返回 -1 或 0，
// 样本里也常有「这一窗口没有值」的 0。把它们当成「最低值」的话，
// 报告会显示「自适应限流最低阈值 0 req/s」，看着像服务挂了，其实是没数据。
func pick(samples []metrics.Sample, key, mode string) (float64, bool) {
	found := false
	best := 0.0
	sum := 0.0
	count := 0

	for _, sample := range samples {
		value, ok := sample.Values[key]
		if !ok {
			continue
		}

		switch mode {
		case modeLowest:
			if value <= 0 {
				continue
			}
			if !found || value < best {
				best = value
				found = true
			}
		case modeAvg:
			sum += value
			count++
			found = true
		default:
			if !found || value > best {
				best = value
				found = true
			}
		}
	}

	if !found {
		return 0, false
	}
	if mode == modeAvg {
		return sum / float64(count), true
	}
	return best, true
}

// ---------- 报告的各个小节 ----------

func writeK6Section(out *strings.Builder, k6 *k6Summary) {
	out.WriteString("## 一、压测客户端看到的结果（k6 视角）\n\n")
	if k6 == nil {
		out.WriteString("没有提供 k6 的 summary 文件，这一节跳过。下次跑压测时加 `--summary-export .tmp/k6_summary.json` 就能补上。\n\n")
		return
	}

	requests, _ := k6.value("http_reqs", "count")
	rate, _ := k6.value("http_reqs", "rate")
	vusMax, hasVUs := k6.value("vus_max", "value")

	out.WriteString("| 项目 | 数值 |\n| --- | --- |\n")
	fmt.Fprintf(out, "| 总请求数 | %s |\n", num(requests))
	fmt.Fprintf(out, "| 平均 QPS（含爬坡段） | %s |\n", f2(rate))
	if hasVUs {
		fmt.Fprintf(out, "| 峰值虚拟用户数 | %s |\n", num(vusMax))
	}
	if failed, ok := k6.value("http_req_failed", "rate"); ok {
		fmt.Fprintf(out, "| 请求失败率 | %s%% |\n", f2(failed*100))
	}

	durations := []struct{ key, label string }{
		{"avg", "平均响应时间"},
		{"p(50)", "P50 响应时间"},
		{"p(95)", "P95 响应时间"},
		{"p(99)", "P99 响应时间"},
		{"max", "最慢一次"},
	}
	for _, item := range durations {
		if value, ok := k6.value("http_req_duration", item.key); ok {
			fmt.Fprintf(out, "| %s | %s ms |\n", item.label, f2(value))
		}
	}
	out.WriteString("\n")

	// 业务口径：k6 脚本自己打的计数器（见 scripts/k6_bench.js）。
	// 光看 HTTP 200 是不够的——「库存不足」「超出限购」这类拒绝也是 200 或 4xx，
	// 只有把它们分开计数，才知道这轮压测到底成功了多少单。
	bizRows := []struct{ metric, label string }{
		{"ok", "抢购成功"},
		{"biz_stock_insufficient", "库存不足（被挡在门外，正常）"},
		{"biz_per_user_limit", "超出每人限购（正常）"},
		{"biz_dup_request_id", "重复请求被幂等拦下（正常）"},
		{"biz_rate_limited", "被限流拒绝（正常）"},
		{"biz_other", "其他业务拒绝"},
		{"conn_err", "连不上服务（异常）"},
	}

	var bizTable strings.Builder
	for _, row := range bizRows {
		if value, ok := k6.value(row.metric, "count"); ok {
			fmt.Fprintf(&bizTable, "| %s | %s |\n", row.label, num(value))
		}
	}
	if bizTable.Len() > 0 {
		out.WriteString("业务口径（k6 脚本按响应内容分类统计）：\n\n")
		out.WriteString("| 结果 | 次数 |\n| --- | --- |\n")
		out.WriteString(bizTable.String())
		out.WriteString("\n")
	}
}

func sampleRow(out *strings.Builder, samples []metrics.Sample, label, key, mode, unit string) {
	value, ok := pick(samples, key, mode)
	if !ok {
		fmt.Fprintf(out, "| %s | 未取到 |\n", label)
		return
	}
	fmt.Fprintf(out, "| %s | %s%s |\n", label, f2(value), unit)
}

func writeServerSection(out *strings.Builder, history *historyResponse, alerts *alertsResponse, snapshot map[string]int64) {
	out.WriteString("## 二、服务端自己看到的（/metrics 视角）\n\n")

	if history == nil {
		out.WriteString("没有取到 `/metrics/history`（服务是不是没启动？）。这一节需要服务在线才能填。\n\n")
	} else if len(history.Samples) == 0 {
		out.WriteString("服务在线，但采样历史还是空的。采样器每 ")
		fmt.Fprintf(out, "%s 秒记一次，", f2(history.IntervalSeconds))
		out.WriteString("**第一次采样只用来打底**（没有上一条数据就算不出速率），所以至少要等两个周期才有数字。压测时间太短的话这里就是空的。\n\n")
	} else {
		samples := history.Samples
		duration := float64(len(samples)) * history.IntervalSeconds
		fmt.Fprintf(out, "采样间隔 %s 秒，共有 %s 个样本（约 %s 分钟）。\n\n",
			f2(history.IntervalSeconds), num(float64(len(samples))), f2(duration/60))

		out.WriteString("| 项目 | 数值 |\n| --- | --- |\n")
		sampleRow(out, samples, "峰值 QPS", metrics.MetricQPS, modePeak, " /s")
		sampleRow(out, samples, "平均 QPS", metrics.MetricQPS, modeAvg, " /s")
		sampleRow(out, samples, "平均响应时间", metrics.MetricAvgLatencyMS, modeAvg, " ms")
		sampleRow(out, samples, "峰值并发（同时没返回的请求数）", metrics.MetricInflight, modePeak, " 个")
		sampleRow(out, samples, "5xx 错误率峰值", metrics.MetricErrorRate, modePeak, " %")
		sampleRow(out, samples, "4xx 拒绝率峰值（限流、重复请求等，属正常拒绝）", metrics.MetricClientErrorRate, modePeak, " %")
		sampleRow(out, samples, "Kafka 消费积压峰值", metrics.MetricKafkaLag, modePeak, " 条")
		sampleRow(out, samples, "在途订单峰值", metrics.MetricPending, modePeak, " 条")
		sampleRow(out, samples, "自适应限流的最低阈值", metrics.MetricAdaptiveLimit, modeLowest, " req/s")
		out.WriteString("\n")
	}

	if snapshot != nil {
		out.WriteString("进程启动至今的累计值（来自 `/metrics`）：\n\n")
		fmt.Fprintf(out, "- 业务请求 %s 个，其中被限流拒绝 %s 个；\n",
			num(float64(snapshot["totalRequests"])), num(float64(snapshot["rateLimited"])))
		fmt.Fprintf(out, "- 秒杀请求 %s 个：落库成功 %s 单，判定失败 %s 单，重复请求 %s 单。\n",
			num(float64(snapshot["seckillRequests"])), num(float64(snapshot["seckillSuccess"])),
			num(float64(snapshot["seckillFailure"])), num(float64(snapshot["seckillDuplicate"])))
		out.WriteString("\n提醒：秒杀请求数是**入口口径**（打进接口就算），成功/失败数是**消费端口径**（真落库了才算）。")
		out.WriteString("两者对不上不代表出错——被限流、被限购拒绝的请求根本没进 Kafka。\n\n")
	}
}

func writeAlertSection(out *strings.Builder, history *historyResponse, alerts *alertsResponse) {
	out.WriteString("## 三、系统自己报出来的异常\n\n")

	if history == nil && alerts == nil {
		out.WriteString("没有取到告警数据（服务没启动？）。\n\n")
		return
	}

	var events []metrics.AlertEvent
	var active []metrics.Alert
	var rules []metrics.RuleStatus
	if alerts != nil {
		events, active, rules = alerts.Events, alerts.Active, alerts.Rules
	} else {
		events, active = history.Events, history.ActiveAlerts
	}

	if len(events) == 0 {
		out.WriteString("这一轮没有发生告警状态变化。可能是真的平稳，也可能是压测时间太短——")
		out.WriteString("告警需要先攒够基线样本（默认每条规则至少 10 个样本，也就是约 20 秒），")
		out.WriteString("而且要求连续几个窗口越线才算数。\n\n")
	} else {
		// /metrics/alerts 返回的事件是「最新的排在最前」，方便页面直接显示；
		// 报告是给人按时间顺序读的，所以这里倒过来，从最早的一条开始写。
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if event.Kind == "resolved" {
				fmt.Fprintf(out, "- `%s` 恢复 **%s**：持续 %s 秒，期间最坏到过 %s。\n",
					event.At.Format("15:04:05"), event.Rule, f2(event.DurationSeconds), f2(event.Peak))
				continue
			}
			fmt.Fprintf(out, "- `%s` 开始告警 **%s**：%s\n", event.At.Format("15:04:05"), event.Rule, event.Reason)
		}
		out.WriteString("\n")
	}

	if len(active) > 0 {
		out.WriteString("报告生成时仍在告警中：\n\n")
		for _, alert := range active {
			fmt.Fprintf(out, "- **%s**：当前 %s（最坏到过 %s），基线 %s，从 %s 起已持续 %s 秒。\n",
				alert.Rule, f2(alert.Value), f2(alert.Peak), f2(alert.Baseline),
				alert.Since.Format("15:04:05"), f2(alert.ElapsedSeconds))
		}
		out.WriteString("\n")
	}

	if len(rules) > 0 {
		out.WriteString("每条规则的体检数据（基线是「平时」的水平，门槛 = 基线与绝对下限里更严的那个）：\n\n")
		out.WriteString("| 规则 | 指标 | 基线 | 波动 | 门槛 | 样本数 | 连续越线 | 状态 |\n")
		out.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, rule := range rules {
			threshold := rule.Baseline + rule.Sigma*rule.StdDev
			if rule.Floor > threshold {
				threshold = rule.Floor
			}
			status := "正常"
			if rule.Active {
				status = "**告警中**"
			}
			fmt.Fprintf(out, "| %s | %s | %s | ±%s | %s | %d | %d | %s |\n",
				rule.Name, rule.Metric, f2(rule.Baseline), f2(rule.StdDev), f2(threshold),
				rule.Samples, rule.Streak, status)
		}
		out.WriteString("\n")
	}
}

// buildHints 是「自动定位瓶颈」的核心：拿几个写死的阈值去套数字。
//
// 为什么写死阈值也行：这些判断本来就是经验值（比如 5xx 超过 1% 肯定不正常、
// 积压超过 1000 条说明消费端追不上），把它们写出来反而比让模型"感觉一下"更可信，
// 而且报告里能同时给出数字和结论，面试时讲得清楚。
func buildHints(facts benchFacts) []string {
	var hints []string

	if facts.history != nil {
		samples := facts.history.Samples
		if value, ok := pick(samples, metrics.MetricErrorRate, modePeak); ok && value > 1 {
			hints = append(hints, fmt.Sprintf("5xx 错误率峰值到了 %s%%（超过 1%%）：有请求真的失败了，去日志里搜 panic 和数据库/Redis 报错。", f2(value)))
		}
		if value, ok := pick(samples, metrics.MetricKafkaLag, modePeak); ok && value > 1000 {
			hints = append(hints, fmt.Sprintf("Kafka 积压峰值 %s 条：订单发进队列了但消费端没跟上，先确认消费者进程还活着、topic 分区数够不够。", num(value)))
		}
		if value, ok := pick(samples, metrics.MetricPending, modePeak); ok && value > 1000 {
			hints = append(hints, fmt.Sprintf("在途订单峰值 %s 条：这段时间 Redis 已经扣了库存、MySQL 还没记账，会出现「页面显示抢到、实际没落库」的窗口期。", num(value)))
		}
		if value, ok := pick(samples, metrics.MetricQPS, modePeak); ok && value > 12000 {
			hints = append(hints, fmt.Sprintf("峰值 QPS %s，已经越过 1 万的目标线：延迟如果再涨，优先看 MySQL 写入和 Redis 是否是瓶颈。", num(value)))
		}
		if value, ok := pick(samples, metrics.MetricClientErrorRate, modePeak); ok && value > 50 {
			hints = append(hints, fmt.Sprintf("4xx 拒绝率峰值 %s%%：大部分请求被限流或业务规则挡掉了。这是秒杀的**设计目标**（把流量挡在数据库门外），不是故障；但要确认挡掉的不是正常用户。", f2(value)))
		}
	}

	if facts.k6 != nil {
		if value, ok := facts.k6.value("conn_err", "count"); ok && value > 0 {
			hints = append(hints, fmt.Sprintf("客户端有 %s 次连接失败：不是业务拒绝，是 TCP 层面没连上，看服务是否被打满、监听队列是否溢出。", num(value)))
		}
		p50, okP50 := facts.k6.value("http_req_duration", "p(50)")
		p99, okP99 := facts.k6.value("http_req_duration", "p(99)")
		if okP50 && okP99 && p50 > 0 && p99/p50 > 10 {
			hints = append(hints, fmt.Sprintf("P99 是 P50 的 %s 倍（%s ms 对 %s ms）：典型的「大部分请求很快、少数请求特别慢」，一般指向锁竞争、GC 停顿或者个别慢查询。", f2(p99/p50), f2(p99), f2(p50)))
		}
	}

	if len(hints) == 0 {
		hints = append(hints, "按上面几条写死的判断规则，这轮没有发现明显异常。")
	}
	return hints
}

func writeHintSection(out *strings.Builder, facts benchFacts) {
	out.WriteString("## 四、瓶颈线索（按固定规则自动判断）\n\n")
	for _, hint := range buildHints(facts) {
		fmt.Fprintf(out, "- %s\n", hint)
	}
	out.WriteString("\n")
}

func writeRawSection(out *strings.Builder, facts benchFacts) {
	out.WriteString("## 五、原始材料（日志里和异常有关的行为）\n\n")

	if strings.TrimSpace(facts.logPath) == "" {
		out.WriteString("没有提供日志文件（-log 参数），这一节跳过。\n\n")
		return
	}
	if facts.logErr != nil {
		fmt.Fprintf(out, "日志读取失败：%v\n\n", facts.logErr)
		return
	}
	if len(facts.logHighlights) == 0 {
		out.WriteString("从日志尾部往回找了 64 MB，没有出现告警、panic、补偿失败这类关键词。\n\n")
		return
	}

	out.WriteString("从日志尾部往回找出来的相关行（按时间先后排列）：\n\n```text\n")
	for _, line := range facts.logHighlights {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteString("```\n\n")
}

// ---------- 组装报告 ----------

type benchFacts struct {
	now      time.Time
	k6       *k6Summary
	history  *historyResponse
	alerts   *alertsResponse
	snapshot map[string]int64

	// 日志只保留「值得抄进报告」的行，不把原文整段塞进来。
	// 压测时的访问日志有几百 MB，整段留着既占内存，写进报告也没人看。
	logHighlights []string
	logErr        error

	k6Path  string
	logPath string
	baseURL string
}

func buildReport(facts benchFacts) string {
	var out strings.Builder

	fmt.Fprintf(&out, "# 压测报告 · %s\n\n", facts.now.Format("2006-01-02 15:04:05"))

	out.WriteString("这份报告由 `cmd/benchreport` 自动生成，把压测客户端、服务端指标和日志三份材料拼在一起。\n\n")
	out.WriteString("素材来源：\n\n")
	fmt.Fprintf(&out, "- k6 summary：%s\n", orNone(facts.k6Path))
	fmt.Fprintf(&out, "- 服务端指标：%s/metrics、/metrics/history、/metrics/alerts\n", facts.baseURL)
	fmt.Fprintf(&out, "- 服务日志：%s\n\n", orNone(facts.logPath))

	writeK6Section(&out, facts.k6)
	writeServerSection(&out, facts.history, facts.alerts, facts.snapshot)
	writeAlertSection(&out, facts.history, facts.alerts)
	writeHintSection(&out, facts)
	writeRawSection(&out, facts)

	out.WriteString("---\n\n")
	out.WriteString("怎么读这份报告（给不熟悉压测的人）：\n\n")
	out.WriteString("1. 先看**第一节**的 P99 和失败率——这是用户真实感受到的；\n")
	out.WriteString("2. 再看**第二节**的 5xx 和积压——这是服务自己扛不扛得住的证据；\n")
	out.WriteString("3. **第三节**是系统主动报出来的异常，比人盯屏幕靠谱；\n")
	out.WriteString("4. **第四节**是自动给出的排查方向，**第五节**是原始日志，方便核对。\n")

	return out.String()
}

func main() {
	var (
		k6Path  = flag.String("k6", "", "k6 --summary-export 导出的 JSON 路径（可省略）")
		baseURL = flag.String("url", "http://127.0.0.1:8080", "服务地址")
		logPath = flag.String("log", "", "服务进程的日志文件路径（可省略）")
		outPath = flag.String("out", "", "报告输出路径，默认 docs/reports/bench-<时间>.md；填 - 表示只打印不写文件")
	)
	flag.Parse()

	now := time.Now()
	facts := benchFacts{
		now:      now,
		k6Path:   *k6Path,
		logPath:  *logPath,
		baseURL:  strings.TrimRight(*baseURL, "/"),
		snapshot: map[string]int64{},
	}

	// 下面的取数都是「能取到就用，取不到就跳过」。这是刻意的：
	// 压测结束后服务可能已经关了，但报告仍然应该能生成（哪怕只剩 k6 那一节）。
	client := &http.Client{Timeout: 5 * time.Second}

	if *k6Path != "" {
		summary, err := loadK6Summary(*k6Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 k6 summary 失败：%v\n", err)
		} else {
			facts.k6 = summary
		}
	}

	var history historyResponse
	if err := fetchJSON(client, facts.baseURL+"/metrics/history", &history); err != nil {
		fmt.Fprintf(os.Stderr, "取 /metrics/history 失败：%v\n", err)
	} else {
		facts.history = &history
	}

	var alerts alertsResponse
	if err := fetchJSON(client, facts.baseURL+"/metrics/alerts", &alerts); err != nil {
		fmt.Fprintf(os.Stderr, "取 /metrics/alerts 失败：%v\n", err)
	} else {
		facts.alerts = &alerts
	}

	if err := fetchJSON(client, facts.baseURL+"/metrics", &facts.snapshot); err != nil {
		fmt.Fprintf(os.Stderr, "取 /metrics 失败：%v\n", err)
		facts.snapshot = nil
	}

	if *logPath != "" {
		highlights, err := readLogHighlights(*logPath, logHighlightLimit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取日志失败：%v\n", err)
			facts.logErr = err
		} else {
			facts.logHighlights = highlights
		}
	}

	report := buildReport(facts)

	if *outPath == "-" {
		fmt.Print(report)
		return
	}
	if *outPath == "" {
		*outPath = filepath.Join("docs", "reports", "bench-"+now.Format("20060102-150405")+".md")
	}
	if dir := filepath.Dir(*outPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "创建目录 %s 失败：%v\n", dir, err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*outPath, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写报告失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("报告已写入 %s\n", *outPath)
}
