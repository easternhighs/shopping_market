// cmd/order 是「订单服务」的入口（阶段 17）。
//
// 它和 cmd/server 的关系：cmd/server 是那个单体，所有模块都装在里面；
// 这一章把跟「买东西」直接相关、数据归属也连在一起的三块
// —— 订单（internal/order）、购物车（internal/cart）、支付（internal/payment）——
// 搬出来单独跑一个进程：
//
//	浏览器 / k6 → 网关（:8081）→ 订单服务（:8085）  /orders、/cart、/payments 走这条
//	                           ↘ 单体    （:8080）  其它接口（/shop、/demo…）还走这条
//
// 为什么这三块要一起搬，而不是各拆一个服务：
// 支付那张表（payments）里存的是「哪张订单付过款」，它离了订单表就没有意义；
// 购物车记录的是「这个用户准备买什么」，和订单共享同一个用户边界、同一段购买流程。
// 放在一个进程里，支付可以直接调用订单服务（进程内函数调用，不走网络），
// 将来要把「改订单状态 + 记支付流水」收进一个数据库事务，也只改代码，不需要跨服务协调。
// 反过来，硬拆成三个服务，一次付款就要跨两次网络，每一跳都可能超时、重试、乱序，
// 换来的不是解耦，而是更多要自己兜底的失败场景。这条判断面试可以直接讲：
// **微服务的边界应该按数据归属和一致性要求来划，不是越细越好。**
//
// 搬过来之后，跟订单有关的一切都归这个进程管：
//   - 下单、查单、付款、取消（/orders/**）；
//   - 购物车的增删改查（/cart/**）；
//   - 支付回调与查询（/payments/**）；
//   - 一个后台任务：库存补偿（阶段 16 的 RetryLoop）—— 下单时没扣成功的库存，
//     在这里按退避时间一遍遍重试，直到扣成功或者重试次数用光。
//
// ⚠ 这个服务【不】提供商品和库存接口：扣库存要走 cmd/product 的
// /internal/stocks/deduct（HTTP），地址由 PRODUCT_SERVICE_URL 指定，
// 不设就默认打 http://127.0.0.1:8084。这就是阶段 16 把那条同库事务剪断的后果：
// 原来是同一个事务里两条 UPDATE，现在是一次可能失败、可能超时、必须自己补偿的网络请求。
//
// 启动方式：
//
//	go run ./cmd/order
//
// 常用环境变量：
//
//	ORDER_ADDR              本服务监听地址，默认 ":8085"
//	TRUST_USER_ID_HEADER    设成 1 / true 表示信任网关写下的 X-User-Id（跑在网关后面时开）
//	PRODUCT_SERVICE_URL     库存服务地址，默认 "http://127.0.0.1:8084"
//	PRODUCT_SERVICE_TIMEOUT 调用库存服务的单次超时，默认 3s
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

	"shopping_market/internal/cart"
	"shopping_market/internal/order"
	"shopping_market/internal/payment"
	"shopping_market/internal/pkg/db"
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/user"
)

func main() {
	addr := envOr("ORDER_ADDR", ":8085")

	// 和另外几个入口一样，用 gin.New() 而不是 gin.Default()：
	// 自带 Logger 会给每个请求同步写一行日志，是入口路径上的同步 I/O。
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())

	// 健康检查：不依赖 MySQL 是否可用，平台用它判断进程还活着。
	// 两个路径并存是刻意为之：/healthz 是本进程的规范路径，
	// /health 是脚本和老入口一直在用的路径，多留一个别名就不用改脚本。
	//
	// 特意把 trust_header 暴露出来：忘了设 TRUST_USER_ID_HEADER 是这一章最容易犯的错，
	// 一眼能看到它到底是 true 还是 false。
	healthHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":       "ok",
			"service":      "order",
			"trace_id":     trace.FromGin(c),
			"trust_header": user.TrustHeaderFromEnv(),
		})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)

	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	// AutoMigrate 是幂等的（表在、列在就什么都不做），所以单体启动时也跑一次同样的迁移
	// 不会冲突 —— 不是重复劳动，而是「谁都可能先启动」的保险。
	//
	// 这里列出的六张表就是这个服务【独占】的数据：
	// orders / order_items / order_outbox 归订单，carts / cart_items 归购物车，
	// payments 归支付。别的服务不该直接写它们。
	if err := dbConn.AutoMigrate(
		&order.Order{},
		&order.Item{},
		&order.OrderOutbox{},
		&cart.Cart{},
		&cart.Item{},
		&payment.Payment{},
	); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// ── 鉴权 ────────────────────────────────────────────────────────────────
	// 订单、购物车、支付三个模块的接口全都要登录，共用这一个中间件。
	// 它的两种模式由环境变量 TRUST_USER_ID_HEADER 决定（实现见 internal/user/auth.go）：
	//   - 默认（没设）：自己验 JWT —— 对应「直连 8085 排查问题」；
	//   - 设成 1 / true：优先采用网关注入的 X-User-Id —— 对应「躲在网关后面」。
	// 正常部署在网关后面时应该打开它，否则每次请求都要多验一遍 JWT。
	authMiddleware := user.AuthMiddleware(user.TrustHeaderFromEnv())
	protected := r.Group("/")
	protected.Use(authMiddleware)

	// ── 订单模块 ────────────────────────────────────────────────────────────
	// 注意第二个参数：跨进程扣库存的客户端。阶段 16 之前，订单服务手里握着的是
	// inventory.Repository —— 一个数据库句柄，扣库存和写订单在同一个事务里。
	// 现在手里只有「一个能发 HTTP 请求的对象」，所以下单必须假设这一跳可能会失败，
	// 补偿逻辑（outbox + RetryLoop）就是为了兜住这个可能性。
	orderRepo := order.NewRepository(dbConn)
	orderService := order.NewService(orderRepo, order.DeductClientFromEnv(orderDeductURL()))
	orderHandler := order.NewHandler(orderService)
	order.RegisterRoutes(protected, orderHandler)

	// ── 购物车模块 ──────────────────────────────────────────────────────────
	cartRepo := cart.NewRepository(dbConn)
	cartService := cart.NewService(cartRepo)
	cartHandler := cart.NewHandler(cartService)
	cart.RegisterRoutes(protected, cartHandler)

	// ── 支付模块 ────────────────────────────────────────────────────────────
	// 支付要改订单状态，所以它拿的是上面那个 orderService —— 同一个进程里的直接调用，
	// 这也正是「这三块该待在同一个服务里」的理由。
	paymentRepo := payment.NewRepository(dbConn)
	paymentService := payment.NewService(paymentRepo, orderService)
	paymentHandler := payment.NewHandler(paymentService)
	payment.RegisterRoutes(protected, paymentHandler)

	// ── 后台任务 ────────────────────────────────────────────────────────────
	// 库存补偿：每 2 秒扫一遍 order_outbox 里「还没扣成功」的便签，
	// 按退避时间重新去商品服务扣库存。没有它，一次网络抖动就会让订单永远卡在
	// pending_stock —— 那是最不该发生的事：用户以为下单成功了，库存却一直没扣。
	//
	// runCtx 挂在信号处理上：收到 Ctrl+C 时先取消它，任务退出后再关 HTTP 服务，
	// 避免「关到一半」的订单被留成孤儿。
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go orderService.RetryLoop(runCtx, order.RetryInterval)

	// ── 启动 ────────────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 和另外几个入口一样的防御性超时：防止「只连上、不发请求」的连接把 goroutine 占满。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[订单服务] 监听 %s，信任 X-User-Id = %v（可用 TRUST_USER_ID_HEADER 打开）",
		addr, user.TrustHeaderFromEnv())
	log.Printf("[订单服务] 独占表 orders / order_items / order_outbox / carts / cart_items / payments")

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("订单服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[订单服务] 收到退出信号，开始优雅关闭...")

	// 先取消后台任务，再关 HTTP：这样补偿任务不会在扣库存扣到一半时被打断。
	stopRun()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[订单服务] 优雅关闭失败: %v", err)
	}
	log.Println("[订单服务] 已退出")
}

// orderDeductURL 返回「本服务扣库存时默认往哪儿打」，只在没设 PRODUCT_SERVICE_URL 时生效。
//
// 为什么这里可以直接写死 8084，而单体那边要写成 orderDefaultDeductURL(cfg.HTTP.Addr) 那种绕的样子：
// 单体要同时支持「单体模式」和「拆开模式」—— 拆开时它自己不再提供 /internal/stocks/deduct，
// 就得改打商品服务；而这个进程是【专门为拆开模式存在】的，商品服务必然在别的进程里，
// 所以默认值就是商品服务的地址，没有第二种情况要判断。
//
// 临时换地址时用环境变量覆盖即可（见 order.DeductClientFromEnv）：
//
//	$env:PRODUCT_SERVICE_URL="http://127.0.0.1:9999"
func orderDeductURL() string {
	return "http://127.0.0.1:8084"
}

// accessLog 和 cmd/server/main.go、cmd/user/main.go、cmd/seckill/main.go、cmd/product/main.go
// 里那几份一样：只在出错（>=400）或特别慢（>=500ms）时打一行，并带上 trace id。
// 正常请求一条都不打，所以入口路径上没有任何同步 I/O。
func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[订单服务] %s %s status=%d cost=%s trace=%s",
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
