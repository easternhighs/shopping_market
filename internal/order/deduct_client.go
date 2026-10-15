package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"shopping_market/internal/pkg/trace"
)

// DeductItem 是「扣库存」这件事对外的输入格式（阶段 16 新增）。
//
// 它和 inventory.DeductItem 长得一样、但两边各留一份，刻意不共用：
// 这是【两个服务之间的接口约定】，不是同一个进程里的内部结构体。
// 真拆成两个代码仓库之后，这种结构只能靠一份接口文档对齐，不可能 import 对方的 struct。
type DeductItem struct {
	SKUID    uint `json:"sku_id"`
	Quantity int  `json:"quantity"`
}

// 跨服务调库存服务时，可能得到的三种结论（前两种是「确定的答案」，第三种是「说不清」）。
var (
	// ErrStockInsufficient 库存服务明确回答「不够」。
	// 这是确定的答案：再重试一万次也是不够，所以订单应该真的撤掉，而不是傻等。
	ErrStockInsufficient = errors.New("库存不足")

	// ErrDeductUnavailable 说不清发生了什么：对方没起来、超时、网络断了、
	// 返回了看不懂的内容…… 这类情况统一当作「暂时不可用，稍后再试」。
	// 注意它和上面那个的区别：这个【不能】用来关单，因为库存可能其实扣成功了。
	ErrDeductUnavailable = errors.New("库存服务暂时不可用")
)

// DeductClient 是订单服务眼里的「库存服务」。
//
// 为什么定义成接口而不是直接拿 http.Client：订单的业务逻辑（Place / 重试任务）
// 只关心「扣成功没有」，不关心是走 HTTP、走 gRPC 还是走别的。
// 定义成接口之后，将来换成别的协议只改这一个文件；
// 写单元测试时也能塞一个假的实现进来，不用真起一个库存服务。
type DeductClient interface {
	Deduct(ctx context.Context, requestID string, items []DeductItem) error
}

// 两个默认值，都可以用环境变量覆盖（见 DeductClientFromEnv）。
const (
	defaultProductTimeout = 3 * time.Second
	// deductPath 是库存服务那边暴露的内部接口，和 internal/inventory/router.go 注册的一致。
	deductPath = "/internal/stocks/deduct"
)

// DeductClientFromEnv 按环境变量装配一个跨服务的扣库存客户端。
//
// fallbackURL 是「没配 PRODUCT_SERVICE_URL 时该往哪儿打」，由调用方决定：
//   - 单体自己就提供库存接口时（没拆 product 服务），打自己的地址；
//   - 单体已经让位（PRODUCT_MODULE=0）时，打 cmd/product 的地址。
//
// PRODUCT_SERVICE_TIMEOUT 控制单次调用的超时，默认 3 秒。
// 为什么必须设超时：不设的话，库存服务一旦「连得上但不回话」，
// 下单请求就会一直挂在那里，用户的浏览器转圈转到天荒地老，
// 后台重试任务也会被这一条卡住、后面的单子全排不上队。
func DeductClientFromEnv(fallbackURL string) DeductClient {
	baseURL := strings.TrimSpace(os.Getenv("PRODUCT_SERVICE_URL"))
	if baseURL == "" {
		baseURL = fallbackURL
	}

	timeout := defaultProductTimeout
	if raw := strings.TrimSpace(os.Getenv("PRODUCT_SERVICE_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			timeout = d
		} else {
			log.Printf("PRODUCT_SERVICE_TIMEOUT=%q 解析失败，使用默认值 %s", raw, defaultProductTimeout)
		}
	}

	log.Printf("[订单服务] 扣库存走 %s%s，单次超时 %s", strings.TrimRight(baseURL, "/"), deductPath, timeout)
	return NewHTTPDeductClient(baseURL, timeout)
}

// NewHTTPDeductClient 用 HTTP 实现跨服务扣库存。
func NewHTTPDeductClient(baseURL string, timeout time.Duration) DeductClient {
	if timeout <= 0 {
		timeout = defaultProductTimeout
	}
	return &httpDeductClient{
		url:    strings.TrimRight(baseURL, "/") + deductPath,
		client: &http.Client{Timeout: timeout},
	}
}

// httpDeductClient 是 DeductClient 的 HTTP 实现。
type httpDeductClient struct {
	url    string
	client *http.Client
}

type deductRequestBody struct {
	RequestID string       `json:"request_id"`
	Items     []DeductItem `json:"items"`
}

// deductResponseBody 只挑我们关心的字段。
// Code 是给机器看的（insufficient_stock / deduct_in_progress / ...），
// Error 是给人看的（中文文案）。靠文案判断等于埋雷，改一个字就悄悄失效。
type deductResponseBody struct {
	Status string `json:"status"`
	Code   string `json:"code"`
	Error  string `json:"error"`
}

// Deduct 请求库存服务扣掉这一单的库存。
//
// request_id 是幂等键：同一次下单重试多少次，库存都只扣第一次那一次，
// 所以这个方法可以放心地被重试任务反复调用。
func (c *httpDeductClient) Deduct(ctx context.Context, requestID string, items []DeductItem) error {
	body, err := json.Marshal(deductRequestBody{RequestID: requestID, Items: items})
	if err != nil {
		return fmt.Errorf("%w: 序列化请求失败: %v", ErrDeductUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: 构造请求失败: %v", ErrDeductUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 把 trace id 原样带上：这样库存服务日志里的那一行能和我们这里的日志对上号，
	// 排查「这个 request_id 到底扣没扣」时不用两边靠时间猜。
	// 阶段 16 的验收标准之一就是「同一个 trace 在 order 与 product 两侧日志能对上」。
	if id := trace.FromContext(ctx); id != "" {
		req.Header.Set(trace.HeaderKey, id)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeductUnavailable, err)
	}
	defer resp.Body.Close()

	// 只读最多 64KB：对方万一返回一个超大响应体，也不能把我们的内存吃掉。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var parsed deductResponseBody
	_ = json.Unmarshal(raw, &parsed)

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil

	case resp.StatusCode == http.StatusConflict && parsed.Code == "insufficient_stock":
		// 库存真的不够：这是确定的答案，调用方应该据此撤单。
		return ErrStockInsufficient

	case resp.StatusCode == http.StatusConflict && parsed.Code == "deduct_in_progress":
		// 同一个 request_id 正在被另一个请求处理。结果是「还不知道」，
		// 绝不能当成失败去撤单，只能过一会儿再问一次。
		return fmt.Errorf("%w: 同一个 request_id 正在处理中", ErrDeductUnavailable)

	default:
		return fmt.Errorf("%w: status=%d body=%s", ErrDeductUnavailable, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}
