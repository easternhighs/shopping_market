package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/product"
	"shopping_market/internal/seckill"
	"shopping_market/internal/shop"
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
		&inventory.DeductRecord{},
		&cart.Cart{},
		&cart.Item{},
		&order.Order{},
		&order.Item{},
		&order.OrderOutbox{},
		&payment.Payment{},
		&seckill.Activity{},
		&seckill.Item{},
		&seckill.Order{},
	); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 超时关单依赖「同一个用户可以重新抢同一个商品」，所以得把老库上
	// seckill_orders 的 (item_id, user_id) 唯一索引改成普通索引。
	// 结构体 tag 改了也不会自动改老库，必须手动修正一次，原因见 migrate.go。
	if err = seckill.EnsureOrderIndexes(dbConn); err != nil {
		log.Fatalf("修正秒杀订单索引失败: %v", err)
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
	// trace 放在最前面：后面每个环节（包括 accessLog 打的那行出错日志）都要用到这个编号。
	// 这也是阶段 13 的遗留项：阶段 13 只把 X-Trace-Id 从网关「发」了出来，
	// 单体作为网关的下游，这里把编号接住——走网关时沿用网关的值，直连 8080 时自己生成。
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())
	appMetrics := metrics.NewMetrics()
	r.Use(metrics.Middleware(appMetrics))

	// 6.3 可观测性（旁路功能，不在抢购链路上）：
	//   - 指标只知道自己「一共」干了多少活，看不出「现在」快不快；
	//   - 采样器每隔 2 秒把累计值换算成速率（每秒多少），攒成一条历史曲线；
	//   - 告警引擎拿当前值和「平时」比，持续偏离就自己报出来。
	// 采样间隔 2 秒、保留 900 个样本（约半小时），够覆盖一次完整的压测。
	alertEngine := metrics.NewEngine(metrics.DefaultRules(), log.Printf)
	sampler := metrics.NewSampler(appMetrics, alertEngine, 2*time.Second, 900)

	// 健康检查：用来确认服务还活着。
	r.GET("/health", healthHandler)
	r.GET("/metrics", gin.WrapH(appMetrics.Handler()))
	metrics.RegisterObservabilityRoutes(r, sampler, alertEngine)

	// 用户模块装配。
	userRepo := user.NewRepository(dbConn)
	userService := user.NewService(userRepo)
	userHandler := user.NewHandler(userService)

	// 鉴权中间件（阶段 14）：所有需要登录的路由共用这一个。
	// 它的两种模式由环境变量 TRUST_USER_ID_HEADER 决定（实现见 internal/user/auth.go）：
	//   - 默认（没设）：老规矩，自己验 JWT —— 对应「单体单独启动、直连 8080」；
	//   - 设成 1 / true：优先采用网关注入的 X-User-Id —— 对应「单体躲在网关后面」。
	authMiddleware := user.AuthMiddleware(user.TrustHeaderFromEnv())

	// USER_MODULE=0 时【不】注册用户路由（阶段 14 演示「真的拆出去了」用）：
	// 这时 /login /register /me 只由 cmd/user 那个独立进程提供，必须过网关才能访问；
	// 单体自己就只剩业务接口了。
	// 默认（不设这个变量）仍然注册，保证「单体服务仍可单独启动」这条验收标准。
	if os.Getenv("USER_MODULE") != "0" {
		user.RegisterRoutes(r, userHandler, authMiddleware)
	}

	// 商品 + 库存模块装配（阶段 16）。
	//
	// 三段式：仓储 → 业务层 → HTTP 层，和别的模块一样。
	// 注意这几个变量【不在】下面的开关里：/demo 和 /shop 两个页面还要直接用
	// productService、inventoryService 去查数据（它们调的是函数，不是 HTTP 接口），
	// 开关只控制「要不要把这些路由注册到这个进程上」。
	productRepo := product.NewRepository(dbConn)
	productService := product.NewService(productRepo)
	productHandler := product.NewHandler(productService)

	inventoryRepo := inventory.NewRepository(dbConn)
	inventoryService := inventory.NewService(inventoryRepo)
	inventoryHandler := inventory.NewHandler(inventoryService)

	// PRODUCT_MODULE=0 时【不】注册商品和库存路由（阶段 16 演示「真的拆出去了」用）：
	//   - /products、/stocks 改由 cmd/product 那个独立进程提供，必须过网关才能访问；
	//   - 内部接口 /internal/stocks/deduct 也跟着走 —— 订单服务按自己配的地址去扣库存。
	// 默认（不设这个变量）仍然注册，保证「单体服务仍可单独启动」这条验收标准。
	if os.Getenv("PRODUCT_MODULE") != "0" {
		product.RegisterRoutes(r, productHandler)
		inventory.RegisterRoutes(r, inventoryHandler)
	}

	// 需要登录的接口组。
	protected := r.Group("/")
	protected.Use(authMiddleware)

	// 购物车模块路由。
	cartRepo := cart.NewRepository(dbConn)
	cartService := cart.NewService(cartRepo)
	cartHandler := cart.NewHandler(cartService)
	cart.RegisterRoutes(protected, cartHandler)

	// 订单模块路由。
	//
	// 阶段 16 起，订单服务不再直接拿 inventory.Repository：库存在另一个进程里，
	// 「同一个事务里扣库存 + 建订单」那条路已经走不通了。
	// 改成注入一个【跨服务的扣库存客户端】，具体打哪个地址见 orderDefaultDeductURL()，
	// 也可以用环境变量 PRODUCT_SERVICE_URL 直接覆盖。
	orderRepo := order.NewRepository(dbConn)
	orderService := order.NewService(orderRepo, order.DeductClientFromEnv(orderDefaultDeductURL(cfg.HTTP.Addr)))
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
	// ⚠ 消费者必须和下面那三个后台任务共用同一个开关，而且是【创建】时就要挡住，
	// 不能只在「启动时」挡：kafka-go 的 NewReader 一创建就加入消费组（源码里直接 go r.run）。
	// 所以 SECKILL_MODULE=0 时如果只是不跑 ConsumeOrders，这 6 个消费者照样会加入消费组、
	// 占住一部分分区却不消费 —— 秒杀服务那边就只剩另一半分区能用，消息一直卡在 Kafka 里。
	// 关掉时干脆一个都不建：队列里没有消费者时 Publish / Settled 照常工作，
	// 关掉开关的进程本来也没有后台任务会用到它们。
	var kafkaConsumers []*kafkapkg.Consumer
	if os.Getenv("SECKILL_MODULE") != "0" {
		kafkaConsumers = make([]*kafkapkg.Consumer, 0, kafkaConsumerCount)
		for i := 0; i < kafkaConsumerCount; i++ {
			kafkaConsumers = append(kafkaConsumers, kafkapkg.NewConsumer(kafkaBrokers, kafkaTopic, kafkaGroupID))
		}
	}
	seckillCache := seckill.NewCache(redisClient)
	seckillQueue := seckill.NewQueue(kafkaProducer, kafkaConsumers...)
	seckillService := seckill.NewService(seckillRepo, seckillCache, seckillQueue)
	seckillService.SetMetricsObserver(appMetrics.ObserveSeckill)
	// 抢购接口的限流在 Redis 脚本里完成，中间件看不到，所以由业务层主动上报指标。
	seckillService.SetRateLimitObserver(appMetrics.ObserveRateLimited)
	// 付款时限：默认 15 分钟；演示时用环境变量 SECKILL_PAY_TIMEOUT 调短（例如 20s），
	// 不用等一刻钟就能看到「超时关单、库存还回去」。
	seckillService.SetPayTimeout(seckillPayTimeout())
	// 自适应限流（6.3）：100 次/秒仍然是硬上限，但系统吃力时会自己往下收紧。
	// 判据是「平均延迟超过 50ms 或开始报 5xx」，规则见 internal/pkg/ratelimit/adaptive.go。
	// 它必须声明在开关【外面】：下面的采样器回调（SetObserver / SetExtra）还要用它。
	adaptiveController := ratelimit.NewController(5, 100, 50*time.Millisecond)

	if os.Getenv("SECKILL_MODULE") != "0" {
		seckillHandler := seckill.NewHandler(seckillService)
		seckillRateLimiter := ratelimit.NewRedisLimiter(redisClient)
		// 限流中间件只挂在秒杀的管理类接口上（建活动、查活动、建商品）。
		// 抢购接口不带中间件：它的限流和库存扣减共用同一次 Redis 往返，
		// 再套一层中间件就等于把刚省下来的往返又加回去（见 internal/seckill/cache.go 的 hotPathLuaScript）。
		seckillGroup := protected.Group("/")
		seckillGroup.Use(ratelimit.RateLimit(ratelimit.NewAdaptiveLimiter(seckillRateLimiter, adaptiveController), 100, time.Second, appMetrics.ObserveRateLimited))
		seckill.RegisterRoutes(seckillGroup, protected, seckillHandler)
	}

	// LAG 探针：每 5 秒问一次 Kafka「消费组落后多少条消息」，把答案留在内存里。
	// 采样器只读缓存，不做网络请求，所以采样节奏不会被 Kafka 拖慢（见 internal/pkg/kafka/lag.go）。
	lagProbe := kafkapkg.NewLagProbe(kafkaBrokers, kafkaTopic, kafkaGroupID, 5*time.Second)

	// 把采样结果接到自适应控制器上：每个窗口结束喂一次「平均延迟 + 错误率」。
	sampler.SetObserver(func(sample metrics.Sample) {
		adaptiveController.Observe(
			sample.Values[metrics.MetricAvgLatencyMS],
			sample.Values[metrics.MetricErrorRate],
		)
	})
	// 再给采样器补上三个它自己看不到的数：
	// Kafka LAG（要问 Kafka）、在途订单数（在秒杀模块里）、当前限流阈值（在限流器里）。
	sampler.SetExtra(func() map[string]float64 {
		return map[string]float64{
			metrics.MetricKafkaLag:      float64(lagProbe.Lag()),
			metrics.MetricPending:       float64(seckillService.Pending()),
			metrics.MetricAdaptiveLimit: float64(adaptiveController.Current()),
		}
	})

	// 演示控制台（阶段 6.2）：把已经搭好的 user / product / inventory / seckill
	// 四个模块串起来，挂在 /demo 下。它是纯新增的旁路功能，不参与抢购热路径；
	// /demo 和 /metrics 的访问也不计入 metrics 的「业务总请求」，
	// 免得演示页自己的轮询污染压测口径（见 internal/pkg/metrics/middleware.go）。
	demoHandler := demo.NewHandler(userService, productService, inventoryService, seckillService)
	// 「一键造数据」会往库里写数据，属于演示专用；启动时设置 DEMO_SEED=0 可以关掉它。
	demoHandler.SetSeedEnabled(os.Getenv("DEMO_SEED") != "0")
	demo.RegisterRoutes(r, demoHandler)

	// 独立前台（阶段 7）：给用户看的商城页面，挂在 /shop 下。
	// 和 /demo 的分工：/demo 讲「系统内部在干什么」，/shop 讲「用户看到了什么」。
	// 它同样是纯新增的旁路功能，不参与抢购热路径；
	// /shop 的访问也和 /demo 一样不计入 metrics 的业务总请求，
	// 免得用户开着页面就把压测口径搅浑（见 internal/pkg/metrics/middleware.go）。
	//
	// 注意第三个参数：把上面那个统一的鉴权中间件传进去，只有 /shop/my（查我的订单）需要登录。
	// 页面本身不挂鉴权——得先能打开页面，才谈得上在里面登录。
	shopHandler := shop.NewHandler(productService, seckillService)
	shop.RegisterRoutes(r, shopHandler, authMiddleware)
	// 启动秒杀消费端：它像一个后台工作人员，不停从 Kafka 拿消息，
	// 再把“已经在 Redis 预扣成功”的请求真正写进 MySQL。
	if os.Getenv("SECKILL_MODULE") != "0" {
		go func() {
			if err := seckillService.ConsumeOrders(context.Background()); err != nil {
				log.Printf("秒杀消费端退出: %v", err)
			}
		}()

		// 启动库存对账任务：每隔一段时间，用 MySQL 的 sold 修正 Redis 库存。
		go seckillService.ReconcileLoop(context.Background(), 30*time.Second)

		// 启动秒杀超时关单任务：到点还没付款的订单会被关掉，占用的库存还回商品池。
		// 它同时走「Redis 延时队列」和「MySQL 兜底扫描」两条路，详见 expire.go。
		go seckillService.ExpireLoop(context.Background())
	}

	// 启动 6.3 的两个后台任务：采样循环和 LAG 探针。
	// 它们都退出得很干脆（ctx 一取消就返回），进程退出时不会拖着不放。
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	lagProbe.Start(runCtx)
	go sampler.Run(runCtx)
	// 启动库存补偿任务（阶段 16）：每 2 秒扫一遍 order_outbox 里「还没扣成功」的便签，
	// 按退避时间重新去商品服务扣库存。没有它，一次网络抖动就会让订单永远卡在
	// pending_stock —— 那是最不该发生的事：用户以为下单成功了，库存却一直没扣。
	go orderService.RetryLoop(runCtx, order.RetryInterval)

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
			log.Printf("[http] %s %s status=%d cost=%s trace=%s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), cost, trace.FromGin(c))
		}
	}
}

// healthHandler 是健康检查接口，用来确认服务还活着。
// Gin 会传入 *gin.Context，我们可以通过它把内容写回客户端。
func healthHandler(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, `{"status":"ok"}`)
}

// orderDefaultDeductURL 决定「订单服务扣库存时，默认往哪儿打」——只在没设
// PRODUCT_SERVICE_URL 时生效：
//   - PRODUCT_MODULE=0：商品 / 库存已经让给 cmd/product 了，打它默认的 :8084；
//   - 其它情况：单体自己就提供 /internal/stocks/deduct，打自己的监听端口。
//
// 为什么要多这一层：同一份 cmd/server 要同时支持「单体模式」和「拆开模式」。
// 拆开时若还硬编码打 8080，8080 那边已经没有这个接口了（会 404），
// 补偿任务会一直重试到次数用光、把订单白白关掉。
//
// selfAddr 是单体的监听地址（形如 ":8080"）。端口从它里面取，
// 这样改了端口的部署也能对上；解析不出来就退回默认的 8080。
func orderDefaultDeductURL(selfAddr string) string {
	if os.Getenv("PRODUCT_MODULE") == "0" {
		return "http://127.0.0.1:8084"
	}

	port := "8080"
	if _, p, err := net.SplitHostPort(selfAddr); err == nil && p != "" {
		port = p
	}
	return "http://127.0.0.1:" + port
}

// seckillPayTimeout 读取环境变量 SECKILL_PAY_TIMEOUT（例如 "20s"、"15m"），
// 得到「订单落库后多久没付款就关单」。没设置或者格式不对就用默认值。
// 演示脚本靠它把一刻钟缩短到几十秒，这样现场就能看到超时关单的完整流程。
func seckillPayTimeout() time.Duration {
	const envKey = "SECKILL_PAY_TIMEOUT"

	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		// 默认值也要打一行日志：不然只看启动日志根本分不清「没配」和「配了但没生效」，
		// 而付款时限直接决定订单多久会被关掉，属于排障时必须一眼能看到的信息。
		log.Printf("秒杀付款时限使用默认值 %s（想改短可设置 %s，例如 30s）", seckill.DefaultPayTimeout, envKey)
		return seckill.DefaultPayTimeout
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("%s=%q 解析失败，使用默认付款时限 %s", envKey, raw, seckill.DefaultPayTimeout)
		return seckill.DefaultPayTimeout
	}

	log.Printf("秒杀付款时限设置为 %s（来自 %s）", d, envKey)
	return d
}
