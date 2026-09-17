package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// requestResult 记录一次压测请求的结果。
type requestResult struct {
	statusCode int
	latency    time.Duration
	err        error
}

// httpClient 是压测工具复用的 HTTP 客户端。
var httpClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	target := flag.String("url", "http://127.0.0.1:8080/health", "压测目标地址")
	method := flag.String("method", "GET", "请求方法")
	body := flag.String("body", "", "请求体")
	token := flag.String("token", "", "Bearer Token，可为空")
	concurrency := flag.Int("c", 100, "并发数")
	duration := flag.Duration("d", 10*time.Second, "压测时长")
	flag.Parse()

	if *target == "" || *concurrency <= 0 || *duration <= 0 {
		log.Fatal("url、并发数、时长参数不合法")
	}

	results := runLoad(*target, *method, *body, *token, *concurrency, *duration)
	printSummary(results, *duration)
}

// runLoad 启动多个 goroutine，持续请求目标地址，直到 duration 结束。
// TODO(你来实现)：
//  1. 创建 stop channel，并启动 concurrency 个 worker goroutine；
//  2. 每个 worker 循环构造 *http.Request，设置 Method、Body、Authorization；
//  3. 调用 httpClient.Do(req)，记录 statusCode、latency、err；
//  4. 使用互斥锁把每个 requestResult 安全地追加到 results；
//  5. duration 结束后关闭 stop，等待所有 worker 退出，返回 results。
func runLoad(target, method, body, token string, concurrency int, duration time.Duration) []requestResult {
	var (
		mu      sync.Mutex
		results []requestResult
		wg      sync.WaitGroup
		stop    = make(chan struct{})
	)

	//将结果添加到results切片中，写入操作需要上锁
	appendResult := func(r requestResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}

	// TODO(你来实现)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				//创建一个新的HTTP请求
				req, err := http.NewRequest(method, target, strings.NewReader(body))
				if err != nil {
					//出现错误则添加进结果并继续下一个
					appendResult(requestResult{err: err})
					continue
				}

				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)

				}

				startTime := time.Now()
				res, err := httpClient.Do(req)
				latency := time.Since(startTime)

				if err != nil {
					appendResult(requestResult{latency: latency, err: err})
					continue
				}

				// 读取响应体，方便复用
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()

				//最终结果
				appendResult(requestResult{
					statusCode: res.StatusCode,
					latency:    latency,
				})
			}
		}()
	}

	// 等待duration期间一直开启concurrency个协程，不断开启HTTP请求
	time.Sleep(duration)
	close(stop)
	wg.Wait()

	return results
}

// printSummary 输出压测汇总：总请求数、QPS、状态码分布、P50/P95/P99。
// TODO(你来实现)：统计 results 并打印关键指标。
func printSummary(results []requestResult, duration time.Duration) {
	// TODO(你来实现)

	requestCount := len(results)
	statusCodeMap := make(map[int]int)
	var (
		latencies      []time.Duration
		errResultCount int
	)

	for _, result := range results {
		// 排除状态码为0和发生错误的请求，单独处理
		if result.statusCode == 0 || result.err != nil {
			errResultCount++
			continue
		}
		statusCodeMap[result.statusCode]++
		latencies = append(latencies, result.latency)
	}

	// QPS计算：总请求数 / 压测持续时长
	QPS := float64(requestCount) / float64(duration.Seconds())
	// 切片排序sort.Slice(slice, func (i, j int) bool { rules })根据给定函数规则进行排序
	// 也可以直接使用slice.Sort
	slices.Sort(latencies)

	fmt.Printf("QPS:%0.2f\n", QPS)
	fmt.Println("状态码分布情况：", statusCodeMap)
	fmt.Println("错误请求数量：", errResultCount)
	fmt.Printf("P50:%v\n", percentile(latencies, 0.50))
	fmt.Printf("P95:%v\n", percentile(latencies, 0.95))
	fmt.Printf("P99:%v\n", percentile(latencies, 0.99))

}

// 获取p位置处的已排序切片sorted元素
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * p) //根据p分位数在对应下标处取整
	return sorted[index]
}
