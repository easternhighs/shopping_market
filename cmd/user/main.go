// cmd/user 是「用户服务」的入口（阶段 14）。
//
// 它和 cmd/server 的关系：cmd/server 是单体，所有模块都装在里面；
// 这里把其中的用户模块（internal/user）搬出来单独跑一个进程。请求的走向变成：
//
//	浏览器 / curl → 网关（:8081）→ 用户服务（:8082）   /login /register /me 走这条
//	                            ↘ 单体    （:8080）   其它接口（/cart、/orders…）还走这条
//
// 注意：internal/user 的业务逻辑一行都没有为拆分改写，
// 它同时被 cmd/server 和 cmd/user import，区别只是「谁把它注册进自己的路由表」。
// 这就是模块化单体的好处——包边界早就画好了，拆进程只是换一个 main 函数。
//
// 拆出去的好处（面试可以讲）：用户服务能单独扩容/单独重启。
// 登录是要验签的（CPU 活），和秒杀抢购的压力根本不是一回事，绑在一起只能一起扩容；
// 它挂了也只影响登录，购物车、订单、秒杀都照常。
//
// 启动方式：
//
//	go run ./cmd/user
//
// 常用环境变量：
//
//	USER_ADDR              本服务监听地址，默认 ":8082"
//	TRUST_USER_ID_HEADER   设成 1 / true 表示信任网关写下的 X-User-Id（跑在网关后面时开）
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
	"shopping_market/internal/pkg/trace"
	"shopping_market/internal/user"
)

func main() {
	addr := envOr("USER_ADDR", ":8082")

	// 和 cmd/server / cmd/gateway 一样，用 gin.New() 而不是 gin.Default()：
	// 自带 Logger 会给每个请求同步写一行日志，是入口路径上的同步 I/O。
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// 阶段 14 顺手补上阶段 13 的遗留项：下游也要接 trace。
	// 阶段 13 只把 X-Trace-Id 从网关「发」了出来，下游没人接、日志里也看不到它。
	// 挂上这个中间件之后，网关和服务打出的日志就带同一个编号了
	// （它沿用上游传来的值，上游没带才新生成）。
	// 必须放在最前面：后面每个环节（包括鉴权失败时打的那行日志）都要用到这个编号。
	r.Use(trace.Middleware())

	// 和 cmd/server 一样，只记录「出错或特别慢」的请求，正常请求一条都不打。
	r.Use(accessLog())

	// 自己的健康检查：不依赖 MySQL 是否可用，平台（docker compose / k8s）用它判断进程还活着。
	// 特意把 trust_header 暴露出来：忘了设 TRUST_USER_ID_HEADER 是这一阶段最容易犯的错，
	// 一眼能看到它到底是 true 还是 false，比翻日志快得多。
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":       "ok",
			"service":      "user",
			"trace_id":     trace.FromGin(c),
			"trust_header": user.TrustHeaderFromEnv(),
		})
	})

	// 连 MySQL：用户服务要自己读用户表，数据库连接是必须的。
	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	// 用户表由用户服务负责维护。AutoMigrate 是幂等的（表在、列在就什么都不做），
	// 所以单体启动时也跑一次同样的迁移不会冲突——不是重复劳动，而是「谁都可能先启动」的保险。
	if err := dbConn.AutoMigrate(&user.User{}); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 用户模块的三层装配，和 cmd/server 里那三行完全一样。
	userRepo := user.NewRepository(dbConn)
	userService := user.NewService(userRepo)
	userHandler := user.NewHandler(userService)

	// 鉴权中间件：两种模式由 TrustHeaderFromEnv 决定（实现见 internal/user/auth.go）。
	// 用户服务的定位是「跑在网关后面」，所以演示/部署时把 TRUST_USER_ID_HEADER 设成 1，
	// 让它直接用网关验签的结论，不必再验一遍。
	authMiddleware := user.AuthMiddleware(user.TrustHeaderFromEnv())
	user.RegisterRoutes(r, userHandler, authMiddleware)

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 和另外两个入口一样的防御性超时：防止「只连上、不发请求」的连接把 goroutine 占满。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[用户服务] 监听 %s，信任 X-User-Id = %v（可用 TRUST_USER_ID_HEADER 打开）", addr, user.TrustHeaderFromEnv())

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("用户服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[用户服务] 收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[用户服务] 优雅关闭失败: %v", err)
	}
	log.Println("[用户服务] 已退出")
}

// accessLog 和 cmd/server/main.go 里那份一样：只在出错或特别慢时打一行，并带上 trace id。
//
// 两个入口各留一份、刻意不抽公共包——它只有十几行，抽出去反而多一层跳转；
// 而且以后网关 / 用户服务 / 单体想记不同的东西时互不影响。
func accessLog() gin.HandlerFunc {
	const slowThreshold = 500 * time.Millisecond

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		cost := time.Since(start)

		if c.Writer.Status() >= http.StatusBadRequest || cost >= slowThreshold {
			log.Printf("[用户服务] %s %s status=%d cost=%s trace=%s",
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
