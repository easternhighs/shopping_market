package main

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/cart"
	"shopping_market/internal/config"
	"shopping_market/internal/inventory"
	"shopping_market/internal/order"
	"shopping_market/internal/payment"
	"shopping_market/internal/pkg/db"
	"shopping_market/internal/product"
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
	); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	r := gin.Default()

	// 健康检查：用来确认服务还活着。
	r.GET("/health", healthHandler)

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
