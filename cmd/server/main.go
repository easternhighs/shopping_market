package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/cart"
	"shopping_market/internal/config"
	"shopping_market/internal/demo"
	"shopping_market/internal/inventory"
	"shopping_market/internal/order"
	"shopping_market/internal/payment"
	"shopping_market/internal/pkg/db"
	kafkapkg "shopping_market/internal/pkg/kafka"
	"shopping_market/internal/pkg/metrics"
	"shopping_market/internal/pkg/ratelimit"
	redispkg "shopping_market/internal/pkg/redis"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 需要做性能分析时，设置环境变量 ENABLE_PPROF=1 再启动（见 pprof.go）。
	enableProfilingIfRequested()

	// 连接 MySQL。
	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	// 根据 User 结构体自动创建 users 表。
	if err := dbConn.AutoMigrate(
		&user.User{},
		&product.Product{},
		&product.SKU{},
		&inventory.Stock{},
		&cart.Cart{},
		&cart.Item{},
		&order.Order{},
		&order.Item{},
		&payment.Payment{},
		&seckill.Activity{},
		&seckill.Item{},
		&seckill.Order{},
	); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 用 gin.New() 而不是 gin.Default()，这是本项目最重要的一处性能修正。
	//
	// gin.Default() = gin.New() + Logger + Recovery，其中 Logger 会把**每一个请求**
	// 都同步写一行访问日志到标准输出。看起来只是[记个日志]，但它是入口路径上的同步 I/O：
	// 压测实测，只把访问日志从[写文件]改成[直接丢弃]，/health 的吞吐从约 6,500/s
	// 涨到约 55,000/s，差了差不多 9 倍。
	//
	// 日志本身没错，错在[每条都同步写]。所以这里改成：
	//   - accessLog：只记录出错或特别慢的请求，保住排查能力又几乎不花钱；
	//   - gin.Recovery()：panic 兜底，这个必须留着，否则一个 panic 会打死整个进程。
	// 另外把运行模式设成 Release，关掉 Gin 在 Debug 模式下的额外输出。
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(accessLog())
	r.Use(gin.Recovery())
	appMetrics := metrics.NewMetrics()
	r.Use(metrics.Middleware(appMetrics))

	// 健康检查：用来确认服务还活着。
	r.GET("/health", healthHandler)
	r.GET("/metrics", gin.WrapH(appMetrics.Handler()))

	// 用户模块路由。
	userRepo := user.NewRepository(dbConn)
	userService := user.NewService(userRepo)
	userHandler := user.NewHandler(userService)
	user.RegisterRoutes(r, userHandler)

	// 商品模块路由。
	productRepo := product.NewRepository(dbConn)
	productService := product.NewService(productRepo)
	productHandler := product.NewHandler(productService)
	product.RegisterRoutes(r, productHandler)

	// 库存模块路由。
	inventoryRepo := inventory.NewRepository(dbConn)
	inventoryService := inventory.NewService(inventoryRepo)
	inventoryHandler := inventory.NewHandler(inventoryService)
	inventory.RegisterRoutes(r, inventoryHandler)

	// 需要登录的接口组。
	protected := r.Group("/")
	protected.Use(userHandler.Auth)

	// 购物车模块路由。
	cartRepo := cart.NewRepository(dbConn)
	cartService := cart.NewService(cartRepo)
	cartHandler := cart.NewHandler(cartService)
	cart.RegisterRoutes(protected, cartHandler)

	// 订单模块路由。
	orderRepo := order.NewRepository(dbConn)
	orderService := order.NewService(orderRepo, inventoryRepo)
	orderHandler := order.NewHandler(orderService)
	order.RegisterRoutes(protected, orderHandler)

	// 支付模块路由。
	paymentRepo := payment.NewRepository(dbConn)
	paymentService := payment.NewService(paymentRepo, orderService)
	paymentHandler := payment.NewHandler(paymentService)
	payment.RegisterRoutes(protected, paymentHandler)

	// 秒杀模块路由。
	seckillRepo := seckill.NewRepository(dbConn)
	redisClient := redispkg.New("127.0.0.1:6379")
	if err := redisClient.Ping(context.Background()); err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}
	kafkaBrokers := []string{"127.0.0.1:9092"}
	kafkaTopic := "seckill_orders"
	kafkaGroupID := "shopping_market_seckill"
	// 消费者数量决定消费端能开几个 goroutine 并发消费，
	// 但它受 topic 分区数限制：1 个分区同一时刻只能有 1 个消费者在干活。
	// 分区数由 deployments/docker-compose.yml 的 KAFKA_NUM_PARTITIONS 决定，
	// 已经建好的 topic 要执行 `make kafka-partitions` 增加分区才会生效。
	const kafkaConsumerCount = 6
	kafkaProducer := kafkapkg.NewProducer(kafkaBrokers, kafkaTopic)
	kafkaConsumers := make([]*kafkapkg.Consumer, 0, kafkaConsumerCount)
	for i := 0; i < kafkaConsumerCount; i++ {
		kafkaConsumers = append(kafkaConsumers, kafkapkg.NewConsumer(kafkaBrokers, kafkaTopic, kafkaGroupID))
	}
	seckillCache := seckill.NewCache(redisClient)
	seckillQueue := seckill.NewQueue(kafkaProducer, kafkaConsumers...)
	seckillService := seckill.NewService(seckillRepo, seckillCache, seckillQueue)
	seckillService.SetMetricsObserver(appMetrics.ObserveSeckill)
	// 抢购接口的限流在 Redis 脚本里完成，中间件看不到，所以由业务层主动上报指标。
	seckillService.SetRateLimitObserver(appMetrics.ObserveRateLimited)
	seckillHandler := seckill.NewHandler(seckillService)
	seckillRateLimiter := ratelimit.NewRedisLimiter(redisClient)
	// 限流中间件只挂在秒杀的管理类接口上（建活动、查活动、建商品）。
	// 抢购接口不带中间件：它的限流和库存扣减共用同一次 Redis 往返，
	// 再套一层中间件就等于把刚省下来的往返又加回去（见 internal/seckill/cache.go 的 hotPathLuaScript）。
	seckillGroup := protected.Group("/")
	seckillGroup.Use(ratelimit.RateLimit(seckillRateLimiter, 100, time.Second, appMetrics.ObserveRateLimited))
	seckill.RegisterRoutes(seckillGroup, protected, seckillHandler)

	// 演示控制台（阶段 6.2）：把已经搭好的 user / product / inventory / seckill
	// 四个模块串起来，挂在 /demo 下。它是纯新增的旁路功能，不参与抢购热路径；
	// /demo 和 /metrics 的访问也不计入 metrics 的「业务总请求」，
	// 免得演示页自己的轮询污染压测口径（见 internal/pkg/metrics/middleware.go）。
	demoHandler := demo.NewHandler(userService, productService, inventoryService, seckillService)
	// 「一键造数据」会往库里写数据，属于演示专用；启动时设置 DEMO_SEED=0 可以关掉它。
	demoHandler.SetSeedEnabled(os.Getenv("DEMO_SEED") != "0")
	demo.RegisterRoutes(r, demoHandler)
	// 启动秒杀消费端：它像一个后台工作人员，不停从 Kafka 拿消息，
	// 再把“已经在 Redis 预扣成功”的请求真正写进 MySQL。
	go func() {
		if err := seckillService.ConsumeOrders(context.Background()); err != nil {
			log.Printf("秒杀消费端退出: %v", err)
		}
	}()

	// 启动库存对账任务：每隔一段时间，用 MySQL 的 sold 修正 Redis 库存。
	go seckillService.ReconcileLoop(context.Background(), 30*time.Second)

	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 下面这几个超时是防御性的。不设置的话，一个只连上、却始终不发完整请求的客户端，
		// 就能一直占着一条连接和对应的 goroutine；这种连接攒多了，服务会被慢慢拖垮。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅退出：收到 Ctrl+C（或 kill 信号）时，先停止接收新连接，
	// 给正在处理中的请求最多 10 秒收尾时间，然后才真正退出，
	// 避免把处理到一半的请求直接掐断。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-quit
		log.Println("收到退出信号，开始优雅关闭...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("优雅关闭失败: %v", err)
		}
	}()

	log.Printf("服务启动，监听地址: %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Println("服务已退出")
}

// accessLog 是[只记录异常]的访问日志中间件。
//
// 和 gin.Default() 自带的 Logger 相比，它只在两种情况输出一行：
//   - 状态码 >= 400：请求被拒绝了，或者服务出错了，值得看一眼；
//   - 耗时超过 500 毫秒：接口明显变慢了，值得看一眼。
//
// 正常请求一条日志都不打，所以入口路径上没有任何同步 I/O。
func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[http] %s %s status=%d cost=%s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), cost)
		}
	}
}

// healthHandler 是健康检查接口，用来确认服务还活着。
// Gin 会传入 *gin.Context，我们可以通过它把内容写回客户端。
func healthHandler(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, `{"status":"ok"}`)
}
