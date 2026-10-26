// cmd/console 是「独立演示控制台」进程（阶段 18，默认 :8087）。
//
// 它提供 /demo、/demo/data、/demo/seed，但不拥有任何数据表。
// 造数据需要的商品、库存、活动、账号，全部通过 HTTP 找兄弟服务完成。
//
// 常用环境变量：
//
//	CONSOLE_ADDR          本服务监听地址，默认 ":8087"
//	USER_SERVICE_URL      用户服务地址，默认 "http://127.0.0.1:8082"
//	PRODUCT_SERVICE_URL   商品服务地址，默认 "http://127.0.0.1:8084"
//	SECKILL_SERVICE_URL   秒杀服务地址，默认 "http://127.0.0.1:8083"
//	DEMO_SEED             设 0 时关闭「一键造数据」接口
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

	"shopping_market/internal/demo"
	"shopping_market/internal/pkg/trace"
)

func main() {
	addr := envOr("CONSOLE_ADDR", ":8087")
	userURL := envOr("USER_SERVICE_URL", "http://127.0.0.1:8082")
	productURL := envOr("PRODUCT_SERVICE_URL", "http://127.0.0.1:8084")
	seckillURL := envOr("SECKILL_SERVICE_URL", "http://127.0.0.1:8083")

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())

	healthHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":              "ok",
			"service":             "console",
			"trace_id":            trace.FromGin(c),
			"user_service_url":    userURL,
			"product_service_url": productURL,
			"seckill_service_url": seckillURL,
		})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)

	client := demo.NewHTTPClient(userURL, productURL, seckillURL, 3*time.Second)
	handler := demo.NewHandler(client, client, client, client)
	handler.SetSeedEnabled(os.Getenv("DEMO_SEED") != "0")
	demo.RegisterRoutes(r, handler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[控制台服务] 监听 %s（用户=%s 商品=%s 秒杀=%s）", addr, userURL, productURL, seckillURL)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("控制台服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[控制台服务] 收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[控制台服务] 优雅关闭失败: %v", err)
	}
	log.Println("[控制台服务] 已退出")
}

func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[控制台服务] %s %s status=%d cost=%s trace=%s",
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
