package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/cart"
	"shopping_market/internal/config"
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

	r := gin.Default()
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
	seckillHandler := seckill.NewHandler(seckillService)
	seckillRateLimiter := ratelimit.NewRedisLimiter(redisClient)
	seckillGroup := protected.Group("/")
	seckillGroup.Use(ratelimit.RateLimit(seckillRateLimiter, 100, time.Second, appMetrics.ObserveRateLimited))
	seckill.RegisterRoutes(seckillGroup, seckillHandler)

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

	log.Printf("服务启动，监听地址: %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

// healthHandler 是健康检查接口，用来确认服务还活着。
// Gin 会传入 *gin.Context，我们可以通过它把内容写回客户端。
func healthHandler(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, `{"status":"ok"}`)
}
