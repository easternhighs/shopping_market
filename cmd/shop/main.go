// cmd/shop 是「独立前台」进程（阶段 18，默认 :8086）。
//
// 它只做三件事：
//   - 把 embed 进二进制的商城 / 秒杀 / 购物车页面发出去；
//   - 组装 /shop/data 要的 JSON；
//   - 校验登录信息后查询「我的秒杀订单」。
//
// 它【不连接】MySQL、Redis、Kafka，也不负责建表。商品和秒杀数据都向兄弟服务要：
//
//	内部模式（单体）：本地适配器 → 进程内函数调用
//	独立模式（本进程）：HTTP 客户端 → :8084 商品服务 / :8083 秒杀服务
//
// 为什么值得单独拆：页面服务是「读多、无状态」的，可以随便加副本；
// 它挂了不会影响下单和抢购，重启也不需要等数据库。
//
// 常用环境变量：
//
//	SHOP_ADDR                 本服务监听地址，默认 ":8086"
//	PRODUCT_SERVICE_URL       商品服务地址，默认 "http://127.0.0.1:8084"
//	SECKILL_SERVICE_URL       秒杀服务地址，默认 "http://127.0.0.1:8083"
//	TRUST_USER_ID_HEADER      设 1/true 时信任网关注入的 X-User-Id
//
// 注意：页面里的 fetch 使用相对路径。经网关打开时交互正常；
// 直接打开 :8086/shop 虽然能看到页面，但登录、购物车等请求会打到本进程，可能 404。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/shop"
	"shopping_market/internal/user"
)

func main() {
	addr := envOr("SHOP_ADDR", ":8086")
	productURL := envOr("PRODUCT_SERVICE_URL", "http://127.0.0.1:8084")
	seckillURL := envOr("SECKILL_SERVICE_URL", "http://127.0.0.1:8083")

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())

	healthHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":               "ok",
			"service":              "shop",
			"trace_id":             trace.FromGin(c),
			"product_service_url":  productURL,
			"seckill_service_url":  seckillURL,
			"trust_user_id_header": user.TrustHeaderFromEnv(),
		})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)

	client := shop.NewHTTPClient(productURL, seckillURL, 3*time.Second)
	handler := shop.NewHandler(client, client)
	authMiddleware := user.AuthMiddleware(user.TrustHeaderFromEnv())
	shop.RegisterRoutes(r, handler, authMiddleware)

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[前台服务] 监听 %s（商品=%s 秒杀=%s）", addr, productURL, seckillURL)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("前台服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[前台服务] 收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[前台服务] 优雅关闭失败: %v", err)
	}
	log.Println("[前台服务] 已退出")
}

func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[前台服务] %s %s status=%d cost=%s trace=%s",
				c.Request.Method, c.Request.URL.Path, c.Writer.Status(), cost, trace.FromGin(c))
		}
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
