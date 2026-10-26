package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shopping_market/internal/inventory"
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

const defaultHTTPClientTimeout = 3 * time.Second

// statusError 保留非 2xx 响应的状态码和响应体。
//
// 为什么不能只返回一个字符串：调用方要区分「用户名已存在（409）」和
// 「库存记录不存在（404）」这类正常业务结果，不能靠匹配错误文案。
type statusError struct {
	StatusCode int
	Body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("下游返回 status=%d body=%s", e.StatusCode, e.Body)
}

// HTTPClient 是独立进程模式下，控制台访问四个下游服务的客户端。
//
// 四个服务共用同一套请求逻辑；区别只是 base URL 和路径不同。
type HTTPClient struct {
	userBaseURL    string
	productBaseURL string
	seckillBaseURL string
	client         *http.Client
}

var (
	_ UserRegistrar = (*HTTPClient)(nil)
	_ ProductStore  = (*HTTPClient)(nil)
	_ StockStore    = (*HTTPClient)(nil)
	_ SeckillAdmin  = (*HTTPClient)(nil)
)

func NewHTTPClient(userBaseURL, productBaseURL, seckillBaseURL string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = defaultHTTPClientTimeout
	}

	return &HTTPClient{
		userBaseURL:    strings.TrimRight(userBaseURL, "/"),
		productBaseURL: strings.TrimRight(productBaseURL, "/"),
		seckillBaseURL: strings.TrimRight(seckillBaseURL, "/"),
		client:         &http.Client{Timeout: timeout},
	}
}

// Register 调用用户服务的 POST /register。
// 409 表示用户名已经存在，翻译成 user.ErrUsernameTaken，让造数流程可以安全跳过。
func (c *HTTPClient) Register(ctx context.Context, username, password string) (*user.User, error) {
	req := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{Username: username, Password: password}

	var resp struct {
		User *user.User `json:"user"`
	}
	if err := c.do(ctx, http.MethodPost, c.userBaseURL+"/register", req, &resp); err != nil {
		if isStatus(err, http.StatusConflict) {
			return nil, user.ErrUsernameTaken
		}
		return nil, err
	}
	return resp.User, nil
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

// CreateProduct 调用商品服务的 POST /products。
func (c *HTTPClient) CreateProduct(ctx context.Context, name, description string, price float64) (*product.Product, error) {
	req := struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		Price       float64 `json:"price"`
	}{Name: name, Description: description, Price: price}

	var resp struct {
		Product *product.Product `json:"product"`
	}
	if err := c.do(ctx, http.MethodPost, c.productBaseURL+"/products", req, &resp); err != nil {
		return nil, err
	}
	return resp.Product, nil
}

// CreateSKU 调用商品服务的 POST /products/:id/skus。
func (c *HTTPClient) CreateSKU(ctx context.Context, productID uint, spec string, price float64) (*product.SKU, error) {
	path := c.productBaseURL + "/products/" + strconv.FormatUint(uint64(productID), 10) + "/skus"
	req := struct {
		Spec  string  `json:"spec"`
		Price float64 `json:"price"`
	}{Spec: spec, Price: price}

	var resp struct {
		SKU *product.SKU `json:"sku"`
	}
	if err := c.do(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, err
	}
	return resp.SKU, nil
}

// FindSKUByProductAndSpec 调用商品服务的内部查找接口。
// 找不到时下游返回 {"sku":null}，这里自然解出 nil，不当作错误。
func (c *HTTPClient) FindSKUByProductAndSpec(ctx context.Context, productID uint, spec string) (*product.SKU, error) {
	req := struct {
		ProductID uint   `json:"product_id"`
		Spec      string `json:"spec"`
	}{ProductID: productID, Spec: spec}

	var resp struct {
		SKU *product.SKU `json:"sku"`
	}
	if err := c.do(ctx, http.MethodPost, c.productBaseURL+"/internal/products/skus/find", req, &resp); err != nil {
		return nil, err
	}
	return resp.SKU, nil
}

// Get 调用库存服务的 GET /stocks/:sku_id。
// 404 是正常业务结果：说明这个 SKU 还没有库存记录，造数流程接下来会创建它。
func (c *HTTPClient) Get(ctx context.Context, skuID uint) (*inventory.Stock, error) {
	path := c.productBaseURL + "/stocks/" + strconv.FormatUint(uint64(skuID), 10)

	var resp struct {
		Stock *inventory.Stock `json:"stock"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, inventory.ErrStockNotFound
		}
		return nil, err
	}
	return resp.Stock, nil
}

// CreateStock 调用库存服务的 POST /stocks。
func (c *HTTPClient) CreateStock(ctx context.Context, skuID uint, quantity int) (*inventory.Stock, error) {
	req := struct {
		SKUID    uint `json:"sku_id"`
		Quantity int  `json:"quantity"`
	}{SKUID: skuID, Quantity: quantity}

	var resp struct {
		Stock *inventory.Stock `json:"stock"`
	}
	if err := c.do(ctx, http.MethodPost, c.productBaseURL+"/stocks", req, &resp); err != nil {
		return nil, err
	}
	return resp.Stock, nil
}

// CreateActivity 调用秒杀服务的内部建活动接口。
func (c *HTTPClient) CreateActivity(ctx context.Context, name string, startAt, endAt time.Time) (*seckill.Activity, error) {
	req := struct {
		Name    string `json:"name"`
		StartAt string `json:"start_at"`
		EndAt   string `json:"end_at"`
	}{
		Name:    name,
		StartAt: startAt.Format(time.RFC3339),
		EndAt:   endAt.Format(time.RFC3339),
	}

	var resp struct {
		Activity *seckill.Activity `json:"activity"`
	}
	if err := c.do(ctx, http.MethodPost, c.seckillBaseURL+"/internal/seckill/activities", req, &resp); err != nil {
		return nil, err
	}
	return resp.Activity, nil
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

// CreateItem 把一个 SKU 加进某场秒杀活动。
func (c *HTTPClient) CreateItem(ctx context.Context, activityID, skuID uint, price float64, totalStock, perUserLimit int) (*seckill.Item, error) {
	path := c.seckillBaseURL + "/internal/seckill/activities/" +
		strconv.FormatUint(uint64(activityID), 10) + "/items"
	req := struct {
		SKUID        uint    `json:"sku_id"`
		Price        float64 `json:"price"`
		TotalStock   int     `json:"total_stock"`
		PerUserLimit int     `json:"per_user_limit"`
	}{SKUID: skuID, Price: price, TotalStock: totalStock, PerUserLimit: perUserLimit}

	var resp struct {
		Item *seckill.Item `json:"item"`
	}
	if err := c.do(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, err
	}
	return resp.Item, nil
}

// ListItems 列出某场活动的全部秒杀商品。
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

// RemainingStock 读取 Redis 里的剩余库存。
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

// WarmUpStock 让秒杀服务把商品信息和库存预热到 Redis。
func (c *HTTPClient) WarmUpStock(ctx context.Context, itemID uint) error {
	path := c.seckillBaseURL + "/internal/seckill/items/" +
		strconv.FormatUint(uint64(itemID), 10) + "/warmup"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Pending 读取 Kafka 里尚未落库的秒杀订单数。
func (c *HTTPClient) Pending(ctx context.Context) (int64, error) {
	var resp struct {
		Pending int64 `json:"pending"`
	}
	if err := c.do(ctx, http.MethodGet, c.seckillBaseURL+"/internal/seckill/pending", nil, &resp); err != nil {
		return 0, err
	}
	return resp.Pending, nil
}

func isStatus(err error, status int) bool {
	var se *statusError
	return errors.As(err, &se) && se.StatusCode == status
}

// do 是所有跨服务请求的唯一出口。
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
	if header := trace.FromContext(ctx); header != "" {
		req.Header.Set(trace.HeaderKey, header)
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
		return &statusError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析响应 JSON 失败: %w", err)
		}
	}

	return nil
}
