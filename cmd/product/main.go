// cmd/product 是「商品服务」的入口（阶段 16）。
//
// 它和 cmd/server 的关系：cmd/server 是那个单体，所有模块都装在里面；
// 这一章把商品（internal/product）和库存（internal/inventory）两块搬出来单独跑一个进程：
//
//	浏览器 / k6 → 网关（:8081）→ 商品服务（:8084）  /products、/stocks     走这条
//	                           ↘ 单体    （:8080）  其它接口（/cart、/orders、/shop…）还走这条
//
// 为什么这两块要一起搬：库存是挂在一个个 SKU 上的，而 SKU 是商品的一部分，
// 它们的表（products / skus / stocks）本来就是一张网里的三个节点。
// 把它们拆到两个进程、再由一个进程去改另一个进程的数据，等于把简单问题搞复杂。
//
// 和商品路由一起搬过来的，还有一个【内部接口】：
//
//	POST /internal/stocks/deduct
//
// 它是给「兄弟服务」用的，不是给浏览器用的：订单服务拆出去之后，
// 扣库存没法再和自己写订单放进同一个数据库事务里，只能发一个 HTTP 请求过来扣。
// 这个接口带幂等键（request_id），同一个键调一百次库存也只扣一次
// （账本表 stock_deducts，见 internal/inventory/deduct.go）。
//
// 为什么这个内部接口是安全的：网关的路由表（internal/gateway/route.go）里没有
// /internal 这个前缀，所以从外网访问它只会得到 404，只有内网里的兄弟服务够得着。
// 演示级别够用，真上生产还要再加「只监听内网 / 白名单 IP」。
//
// 启动方式：
//
//	go run ./cmd/product
//
// 常用环境变量：
//
//	PRODUCT_ADDR   本服务监听地址，默认 ":8084"
//	PRODUCT_MODULE 是【单体那边】的开关；本进程不读它，这里列出来只是提醒：
//	               单体设了 PRODUCT_MODULE=0 之后才会把这两个模块的路由让出来。
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

	"shopping_market/internal/inventory"
	"shopping_market/internal/pkg/db"
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/product"
)

func main() {
	addr := envOr("PRODUCT_ADDR", ":8084")

	// 和另外几个入口一样，用 gin.New() 而不是 gin.Default()：
	// 自带 Logger 会给每个请求同步写一行日志，是入口路径上的同步 I/O
	// （见 cmd/server/main.go 顶部那段解释）。
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())

	// 健康检查：不依赖 MySQL 是否可用，平台（docker compose / k8s）用它判断进程还活着。
	// 注册两个路径和 cmd/user、cmd/seckill 保持一致：
	//   - /healthz 是这个进程自己的规范路径；
	//   - /health  是老入口一直在用的路径，脚本不用改就能打。
	healthHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":   "ok",
			"service":  "product",
			"trace_id": trace.FromGin(c),
		})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)

	// 连 MySQL：商品和库存都要自己读库，连接是必须的。
	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	// AutoMigrate 是幂等的（表在、列在就什么都不做），所以单体启动时也跑一次同样的迁移
	// 不会冲突——不是重复劳动，而是「谁都可能先启动」的保险。
	// 注意这里比单体多一张 stock_deducts：它是库存侧的幂等账本，只有这个进程会用到。
	if err := dbConn.AutoMigrate(
		&product.Product{},
		&product.SKU{},
		&inventory.Stock{},
		&inventory.DeductRecord{},
	); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 商品模块装配。
	productRepo := product.NewRepository(dbConn)
	productService := product.NewService(productRepo)
	productHandler := product.NewHandler(productService)
	product.RegisterRoutes(r, productHandler)

	// 库存模块装配。它的路由里就包含那个给订单服务用的 /internal/stocks/deduct。
	inventoryRepo := inventory.NewRepository(dbConn)
	inventoryService := inventory.NewService(inventoryRepo)
	inventoryHandler := inventory.NewHandler(inventoryService)
	inventory.RegisterRoutes(r, inventoryHandler)

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 和另外几个入口一样的防御性超时：防止「只连上、不发请求」的连接把 goroutine 占满。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[商品服务] 监听 %s（商品 + 库存，含内部扣减接口 %s）", addr, "/internal/stocks/deduct")

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("商品服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[商品服务] 收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[商品服务] 优雅关闭失败: %v", err)
	}
	log.Println("[商品服务] 已退出")
}

// accessLog 和 cmd/server/main.go、cmd/user/main.go、cmd/seckill/main.go 里那几份一样：
// 只在出错（>=400）或特别慢（>=500ms）时打一行，并带上 trace id。
// 正常请求一条都不打，所以入口路径上没有任何同步 I/O。
func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[商品服务] %s %s status=%d cost=%s trace=%s",
				c.Request.Method, c.Request.URL.Path, c.Writer.Status(), cost, trace.FromGin(c))
		}
	}
}

// envOr 读环境变量，没有（或只有空格）就用默认值。
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
