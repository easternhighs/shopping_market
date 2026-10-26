// cmd/seckill 是「秒杀服务」的入口（阶段 15）。
//
// 它和 cmd/server 的关系：cmd/server 是那个单体，所有模块都装在里面；
// 这一章把其中最烫手的一块 —— 秒杀（internal/seckill）—— 搬出来单独跑一个进程：
//
//	浏览器 / k6 → 网关（:8081）→ 秒杀服务（:8083）   /seckill/**、/metrics/** 走这条
//	                           ↘ 单体    （:8080）   其它接口（/cart、/orders、/shop…）还走这条
//
// 为什么拿秒杀开刀：它是全项目的核心亮点，也是压力最大的地方。
// 所以这一章有一条别处没有的硬性验收标准 —— **搬完之后必须复测 1 万 QPS**。
// 拆它要证明的不是「拆了更快」（同一条机器上多进程通常更慢，见 docs/microservices.md 第 7 节），
// 而是「拆了没有慢到不可接受，而且热路径一跳都没多」。
//
// 「搬家，不是重写」：internal/seckill 里的业务代码
// （service.go / cache.go / queue.go / expire.go / reconcile.go / worker.go）一行都不改。
// 这个 main 函数做的事，就是把原来写在 cmd/server 里的「秒杀模块装配 + 后台任务启动」
// 原样搬过来，换一个监听端口。
//
// 搬过来之后，跟秒杀有关的一切都归这个进程管：
//   - 抢购接口和管理类接口（/seckill/**）；
//   - 指标与可观测性（/metrics/**）—— 因为网关的路由表把 /metrics 划给了 seckill 服务，
//     所以阶段 6.3 那一整套（指标、采样器、告警、自适应限流、Kafka LAG 探针）也跟着搬了过来；
//   - 三个后台任务：Kafka 消费者、库存对账、超时关单。
//
// 启动方式：
//
//	go run ./cmd/seckill
//
// 常用环境变量：
//
//	SECKILL_ADDR                  本服务监听地址，默认 ":8083"
//	SECKILL_TRUST_USER_ID_HEADER  设成 1 / true 表示信任网关写下的 X-User-Id
//	SECKILL_PAY_TIMEOUT           付款时限（例如 30s），演示超时关单时用
//	SECKILL_REDIS_ADDR            Redis 地址，默认 "127.0.0.1:6379"
//	SECKILL_KAFKA_BROKERS         Kafka 地址（逗号分隔），默认 "127.0.0.1:9092"
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

	"shopping_market/internal/pkg/db"
	kafkapkg "shopping_market/internal/pkg/kafka"
	"shopping_market/internal/pkg/metrics"
	"shopping_market/internal/pkg/ratelimit"
	redispkg "shopping_market/internal/pkg/redis"
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/seckill"
	"shopping_market/internal/user"
)

const (
	// kafkaConsumerCount 是消费端要开几个 goroutine 去拉消息。
	// 它受 topic 分区数限制：1 个分区同一时刻只会有 1 个消费者在干活，
	// 开再多也只是空转。分区数由 deployments/docker-compose.yml 的
	// KAFKA_NUM_PARTITIONS 决定，已经建好的 topic 要跑 make kafka-partitions。
	kafkaConsumerCount = 6

	// reconcileInterval 是库存对账的周期：每隔这么久，拿 MySQL 的 sold
	// 修正一次 Redis 里的剩余库存。
	reconcileInterval = 30 * time.Second

	kafkaTopic   = "seckill_orders"
	kafkaGroupID = "shopping_market_seckill"
)

func main() {
	addr := envOr("SECKILL_ADDR", ":8083")

	// 和另外几个入口一样，用 gin.New() 而不是 gin.Default()：
	// 自带 Logger 会给每个请求同步写一行日志，是入口路径上的同步 I/O。
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gin.Recovery())

	// 指标中间件：给每个请求记一笔（请求数、并发数、耗时、状态码）。
	// 它自己会跳过 /metrics 这类「观测流量」，免得演示页面自己的轮询把压测口径搅浑
	// （见 internal/pkg/metrics/middleware.go 的 ignoredPrefixes）。
	appMetrics := metrics.NewMetrics()
	r.Use(metrics.Middleware(appMetrics))

	// 健康检查：不依赖 MySQL / Redis 是否可用，平台用它判断进程还活着。
	// 特意把 trust_header 暴露出来：忘了设 SECKILL_TRUST_USER_ID_HEADER
	// 是这一章最容易犯的错，一眼能看到它到底是 true 还是 false。
	// 注册了两个路径，是刻意为之：
	//   - /healthz 是这个进程自己的规范路径；
	//   - /health  是单体（cmd/server）一直在用的路径，压测脚本 scripts/k6_bench.js
	//     的 health 模式也打的它。多留一个别名，同一份脚本就能直接拿来压秒杀服务。
	healthHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":       "ok",
			"service":      "seckill",
			"trace_id":     trace.FromGin(c),
			"trust_header": seckillTrustHeader(),
		})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/health", healthHandler)

	// 可观测性三件套（阶段 6.3 原样搬过来）：
	//   - 指标只知道自己「一共」干了多少活，看不出「现在」快不快；
	//   - 采样器每隔 2 秒把累计值换算成速率（每秒多少），攒成一条历史曲线；
	//   - 告警引擎拿当前值和「平时」比，持续偏离就自己报出来。
	alertEngine := metrics.NewEngine(metrics.DefaultRules(), log.Printf)
	sampler := metrics.NewSampler(appMetrics, alertEngine, 2*time.Second, 900)
	r.GET("/metrics", gin.WrapH(appMetrics.Handler()))
	metrics.RegisterObservabilityRoutes(r, sampler, alertEngine)

	// 连 MySQL：秒杀的订单、活动、商品最终都要落在这里。
	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	// 只迁移秒杀自己的三张表。
	// 为什么不把 cmd/server 里那一长串 AutoMigrate 全抄过来：
	// 那等于让秒杀服务去管别人的表，「数据自治」就没了。
	// AutoMigrate 是幂等的，谁先启动跑一次都一样，不会冲突。
	if err = dbConn.AutoMigrate(&seckill.Activity{}, &seckill.Item{}, &seckill.Order{}); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 超时关单依赖「同一个用户可以重新抢同一个商品」，
	// 所以得把老库上 seckill_orders 的 (item_id, user_id) 唯一索引改成普通索引。
	// 结构体 tag 改了也不会自动改老库，必须手动修正一次（原因见 migrate.go）；它是幂等的。
	if err = seckill.EnsureOrderIndexes(dbConn); err != nil {
		log.Fatalf("修正秒杀订单索引失败: %v", err)
	}

	// 连 Redis：抢购的「闸门」——限流、判重、预扣库存都在这里，
	// 而且三件事共用同一次往返，这是压到 1 万 QPS 的关键。
	redisClient := redispkg.New(envOr("SECKILL_REDIS_ADDR", "127.0.0.1:6379"))
	if err = redisClient.Ping(context.Background()); err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}

	// 连 Kafka：抢购成功只是「先在 Redis 占了个坑」，真正写进 MySQL 靠它把消息捎给消费端。
	brokers := splitBrokers(envOr("SECKILL_KAFKA_BROKERS", "127.0.0.1:9092"))
	kafkaProducer := kafkapkg.NewProducer(brokers, kafkaTopic)
	kafkaConsumers := make([]*kafkapkg.Consumer, 0, kafkaConsumerCount)
	for i := 0; i < kafkaConsumerCount; i++ {
		kafkaConsumers = append(kafkaConsumers, kafkapkg.NewConsumer(brokers, kafkaTopic, kafkaGroupID))
	}

	// 秒杀模块的三层装配，和 cmd/server 里的那几行完全一样。
	seckillRepo := seckill.NewRepository(dbConn)
	seckillCache := seckill.NewCache(redisClient)
	seckillQueue := seckill.NewQueue(kafkaProducer, kafkaConsumers...)
	seckillService := seckill.NewService(seckillRepo, seckillCache, seckillQueue)

	// 把秒杀结果报给指标系统。
	// 为什么必须手动接一次：抢购接口的成功/失败/重复/限流都发生在 Redis 脚本内部，
	// 限流中间件根本看不到，只能由业务层主动上报。
	seckillService.SetMetricsObserver(appMetrics.ObserveSeckill)
	seckillService.SetRateLimitObserver(appMetrics.ObserveRateLimited)

	// 付款时限：默认 15 分钟；演示时用 SECKILL_PAY_TIMEOUT 调短（例如 20s），
	// 不用等一刻钟就能看到「超时关单、库存还回去」。
	// ⚠ 微服务的代价在这里露头：单体里的 /shop 页面也要显示同一个倒计时，
	//   所以两个进程【必须设成同一个值】，否则页面上的时间和真正关单的时间会对不上。
	seckillService.SetPayTimeout(seckillPayTimeout())

	// ── 路由 ────────────────────────────────────────────────────────────────
	// 鉴权：本服务的定位是「跑在网关后面」，所以正式部署时应该设
	// SECKILL_TRUST_USER_ID_HEADER=1，直接用网关验签的结论。
	// 但默认值必须是「不信任」（见 seckillTrustHeader）：直接压 8083 做基线时
	// 必须自己验 JWT，否则基线不可比（少了一次验签的 CPU 开销），也不安全。
	authMiddleware := user.AuthMiddleware(seckillTrustHeader())
	protected := r.Group("/")
	protected.Use(authMiddleware)

	seckillHandler := seckill.NewHandler(seckillService)
	seckillRateLimiter := ratelimit.NewRedisLimiter(redisClient)
	// 自适应限流：100 次/秒是硬上限，系统吃力时会自己往下收紧。
	// 判据是「平均延迟超过 50ms 或开始报 5xx」（见 internal/pkg/ratelimit/adaptive.go）。
	adaptiveController := ratelimit.NewController(5, 100, 50*time.Millisecond)
	// 限流中间件只挂在管理类接口（建活动、查活动、建商品）上；
	// 抢购接口不带它 —— 它的限流和库存扣减共用同一次 Redis 往返，
	// 再套一层中间件就等于把刚省下来的那次往返又加回去（见 cache.go 的 hotPathLuaScript）。
	seckillGroup := protected.Group("/")
	seckillGroup.Use(ratelimit.RateLimit(
		ratelimit.NewAdaptiveLimiter(seckillRateLimiter, adaptiveController),
		100, time.Second, appMetrics.ObserveRateLimited,
	))
	seckill.RegisterRoutes(seckillGroup, protected, seckillHandler)
	// 独立模式下的 shop / console 通过这些接口读活动、库存和订单。
	seckill.RegisterInternalRoutes(r, seckillHandler)

	// runCtx 是三个后台任务的「总开关」：进程要退出时取消它，
	// 它们就会各自收尾返回，而不是被硬生生掐断。
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()

	//kafka消费者
	go func() {
		if err := seckillService.ConsumeOrders(runCtx); err != nil {
			log.Printf("秒杀消费端退出: %v", err)
		}
	}()
	//go协程库存对账 和超时关单
	go seckillService.ReconcileLoop(runCtx, reconcileInterval)
	go seckillService.ExpireLoop(runCtx)

	// ── 观测任务的接线 ──────────────────────────────────────────────────────
	// LAG 探针：每 5 秒问一次 Kafka「消费组落后多少条消息」，答案留在内存里。
	// 采样器只读缓存、不自己做网络请求，所以采样节奏不会被 Kafka 拖慢。
	lagProbe := kafkapkg.NewLagProbe(brokers, kafkaTopic, kafkaGroupID, 5*time.Second)
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
	lagProbe.Start(runCtx)
	go sampler.Run(runCtx)

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

	log.Printf("[秒杀服务] 监听 %s，信任 X-User-Id = %v，付款时限 %s",
		addr, seckillTrustHeader(), seckillService.PayTimeout())
	log.Printf("[秒杀服务] MySQL/Redis 已连接，Kafka %v topic=%s group=%s", brokers, kafkaTopic, kafkaGroupID)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("秒杀服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[秒杀服务] 收到退出信号，开始优雅关闭...")

	// 先取消后台任务，再关 HTTP：这样消费端不会在落库落了一半时被打断。
	stopRun()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[秒杀服务] 优雅关闭失败: %v", err)
	}
	log.Println("[秒杀服务] 已退出")
}

// seckillTrustHeader 决定「本服务要不要信任网关注入的 X-User-Id 头」。
// 默认必须是【关】：信任头本身没有任何防伪能力，默认打开的话，
// 任何人手写一个 "X-User-Id: 15" 就能免登录抢购、还能抢别人的限购名额。
func seckillTrustHeader() bool {
	rawUserIDHeader := os.Getenv("SECKILL_TRUST_USER_ID_HEADER")

	userIDHeader := strings.TrimSpace(rawUserIDHeader)

	return userIDHeader == "1" || strings.EqualFold(userIDHeader, "true")
}

// accessLog 和 cmd/server/main.go、cmd/user/main.go 里那两份一样：
// 只在出错（>=400）或特别慢（>=500ms）时打一行，并带上 trace id。
// 正常请求一条都不打，所以入口路径上没有任何同步 I/O。
func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[秒杀服务] %s %s status=%d cost=%s trace=%s",
				c.Request.Method, c.Request.URL.Path, c.Writer.Status(), cost, trace.FromGin(c))
		}
	}
}

// seckillPayTimeout 读取环境变量 SECKILL_PAY_TIMEOUT（例如 "20s"、"15m"），
// 得到「订单落库后多久没付款就关单」。没设置或者格式不对就用默认值。
//
// 这段逻辑在 cmd/server/main.go 里也有一份。两份刻意都留着：
// 它只有十几行，抽成公共包反而多一层跳转；而且两个进程将来想用不同的默认值时互不影响。
// ⚠ 但演示时【两个进程要设同一个值】——不然 /shop 页面上的倒计时
//
//	和这个进程真正关单的时间会对不上（页面是单体渲染的）。
func seckillPayTimeout() time.Duration {
	const envKey = "SECKILL_PAY_TIMEOUT"

	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
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

// splitBrokers 把 "host1:9092,host2:9092" 这种写法拆成切片，顺手去掉空项。
// 为什么允许逗号分隔：Kafka 的地址本来就可以写多个，将来落到多台机器上时不用改代码。
func splitBrokers(raw string) []string {
	parts := strings.Split(raw, ",")
	brokers := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			brokers = append(brokers, part)
		}
	}
	if len(brokers) == 0 {
		return []string{"127.0.0.1:9092"}
	}
	return brokers
}

// envOr 读环境变量，没有（或只有空格）就用默认值。
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
