package shop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
)

const defaultHTTPClientTimeout = 3 * time.Second

// HTTPClient 是「独立进程模式」下，前台眼里的商品服务和秒杀服务。
//
// 一个类型同时实现 ProductReader 和 SeckillReader，是因为两个下游服务的
// 请求方式完全一样（超时、trace、读响应体、解析 JSON）。共用一套底层代码，
// 就不用把同一段容易写错的逻辑复制两遍。
type HTTPClient struct {
	productBaseURL string
	seckillBaseURL string
	client         *http.Client
}

var (
	_ ProductReader = (*HTTPClient)(nil)
	_ SeckillReader = (*HTTPClient)(nil)
)

// NewHTTPClient 创建跨进程的前台数据客户端。
//
// timeout <= 0 时使用 3 秒默认值。这个超时必须存在：
// 下游一旦「连得上但不回话」，没有超时的请求会一直占着 goroutine，
// 页面刷新几次就能把服务拖住。
func NewHTTPClient(productBaseURL, seckillBaseURL string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = defaultHTTPClientTimeout
	}

	return &HTTPClient{
		productBaseURL: strings.TrimRight(productBaseURL, "/"),
		seckillBaseURL: strings.TrimRight(seckillBaseURL, "/"),
		client:         &http.Client{Timeout: timeout},
	}
}

// List 调用商品服务的 GET /products。
func (c *HTTPClient) List(ctx context.Context) ([]product.Product, error) {
	var resp struct {
		Products []product.Product `json:"products"`
	}
	if err := c.do(ctx, http.MethodGet, c.productBaseURL+"/products", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Products, nil
}

// ListSKUsByProductIDs 调用商品服务的批量规格内部接口。
//
// 参数用逗号拼成 product_ids=1,2,3，和 cmd/product 注册的接口约定一致。
// 这里的 ID 全是数字，不需要额外 URL 转义。
func (c *HTTPClient) ListSKUsByProductIDs(ctx context.Context, ids []uint) ([]product.SKU, error) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(uint64(id), 10))
	}

	path := c.productBaseURL + "/internal/products/skus"
	if len(parts) > 0 {
		path += "?product_ids=" + strings.Join(parts, ",")
	}

	var resp struct {
		SKUs []product.SKU `json:"skus"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.SKUs, nil
}

// ListActivities 调用秒杀服务的内部活动列表接口。
func (c *HTTPClient) ListActivities(ctx context.Context) ([]seckill.Activity, error) {
	var resp struct {
		Activities []seckill.Activity `json:"activities"`
	}
	if err := c.do(ctx, http.MethodGet, c.seckillBaseURL+"/internal/seckill/activities", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Activities, nil
}

// ListItems 调用秒杀服务的内部接口，拿某场活动的全部秒杀商品。
func (c *HTTPClient) ListItems(ctx context.Context, activityID uint) ([]seckill.Item, error) {
	path := c.seckillBaseURL + "/internal/seckill/activities/" +
		strconv.FormatUint(uint64(activityID), 10) + "/items"

	var resp struct {
		Items []seckill.Item `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// RemainingStock 读取秒杀商品在 Redis 里的剩余库存。
func (c *HTTPClient) RemainingStock(ctx context.Context, itemID uint) (int, bool, error) {
	path := c.seckillBaseURL + "/internal/seckill/items/" +
		strconv.FormatUint(uint64(itemID), 10) + "/stock"

	var resp struct {
		RemainingStock int  `json:"remaining_stock"`
		StockReady     bool `json:"stock_ready"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return 0, false, err
	}
	return resp.RemainingStock, resp.StockReady, nil
}

// PayTimeout 读取秒杀服务的付款时限配置。
func (c *HTTPClient) PayTimeout(ctx context.Context) (time.Duration, error) {
	var resp struct {
		PayTimeoutSeconds int64 `json:"pay_timeout_seconds"`
	}
	if err := c.do(ctx, http.MethodGet, c.seckillBaseURL+"/internal/seckill/config", nil, &resp); err != nil {
		return 0, err
	}
	return time.Duration(resp.PayTimeoutSeconds) * time.Second, nil
}

// ListUserOrders 查询某个用户的秒杀订单。
func (c *HTTPClient) ListUserOrders(ctx context.Context, userID uint) ([]seckill.Order, error) {
	path := c.seckillBaseURL + "/internal/seckill/users/" +
		strconv.FormatUint(uint64(userID), 10) + "/orders"

	var resp struct {
		Orders []seckill.Order `json:"orders"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Orders, nil
}

// do 是所有跨服务请求的唯一出口：发请求、带 trace、检查状态码、解析 JSON。
func (c *HTTPClient) do(ctx context.Context, method, url string, body any, out any) error {
	var requestBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		requestBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if traceID := trace.FromContext(ctx); traceID != "" {
		req.Header.Set(trace.HeaderKey, traceID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求下游失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("读取响应体失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析响应 JSON 失败: %w", err)
		}
	}
	return nil
}
