// cmd/gateway 是「API 网关」的入口（阶段 13）。
//
// 它和 cmd/server 的关系：server 是业务服务（所有模块都在里面），
// gateway 站在它前面当统一入口。两者是两个独立的进程，各自监听一个端口：
//
//	浏览器 / curl → 网关（:8081）→ 业务服务（:8080）
//
// 本阶段网关是「旁路式」的：server 一行代码都不用改，
// 网关挂了也不影响直连 8080 的压测和演示。
//
// 启动方式：
//
//	go run ./cmd/gateway
//
// 常用环境变量：
//
//	GATEWAY_ADDR                  网关监听地址，默认 ":8081"
//	GATEWAY_BACKEND               默认上游地址，默认 "http://127.0.0.1:8080"
//	GATEWAY_UPSTREAM_<服务名大写>  给某个服务单独指定上游，例如
//	                              GATEWAY_UPSTREAM_USER=http://127.0.0.1:8082
//	                              （阶段 14 把用户服务拆成独立进程时就用它）
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

	"shopping_market/internal/gateway"
	"shopping_market/internal/pkg/trace"
)

func main() {
	addr := envOr("GATEWAY_ADDR", ":8081")
	backend := envOr("GATEWAY_BACKEND", "http://127.0.0.1:8080")

	// 把路由表里出现过的服务都算出一个上游地址。
	// 没单独配环境变量的服务，全部指回 backend（也就是还在同一个进程里的业务服务）。
	upstreams := make(map[string]string)
	for _, route := range gateway.Routes {
		if _, ok := upstreams[route.Service]; ok {
			continue
		}
		upstreams[route.Service] = gateway.ServiceUpstream(route.Service, backend)
	}

	proxy, err := gateway.NewProxy(upstreams)
	if err != nil {
		log.Fatalf("构造反向代理失败: %v", err)
	}

	// 用 gin.New() 而不是 gin.Default()，理由和 cmd/server 里写的一样：
	// 自带 Logger 会给每个请求同步写一行日志，是入口路径上的同步 I/O。
	// 网关这边有自己的一行日志（下面 accessLog），带 trace id，信息比它全。
	r := gin.New()
	r.Use(gin.Recovery())

	// 中间件的顺序就是请求的执行顺序，这里刻意和「谁先谁后」的道理对齐：
	//   trace 在最前面 —— 后面每个环节都要用编号，包括鉴权失败时的日志；
	//   鉴权在代理之前 —— 先把「你是谁」弄清楚，再决定让不让你进去；
	//   最后才是转发 —— 前面通过了才轮到它。
	r.Use(trace.Middleware())
	r.Use(accessLog())
	r.Use(gateway.Auth())

	// 网关自己的健康检查：不转发给任何后端，直接自己回答。
	// 为什么要有它：以后用 docker compose / k8s 编排时，平台需要知道「网关进程还活着吗」，
	// 而这个答案不应该依赖后端服务是否健康——后端挂了不代表网关挂了。
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"service":   "gateway",
			"trace_id":  trace.FromGin(c),
			"upstreams": upstreams,
		})
	})

	// 其余所有路径都交给「路由表 + 反向代理」。
	// 用 NoRoute 而不是逐个注册路由：网关的职责是转发，不需要认识每一个具体接口。
	// 新加接口时，只要它落在已有前缀下（比如新的 /cart/xxx），网关自动就能转，
	// 不用回来改网关——这也是网关只按「前缀」路由的原因。
	r.NoRoute(proxy.Handler())

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 和 cmd/server 一样的防御性超时：防止「只连上不发请求」的连接把 goroutine 占满。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[网关] 监听 %s，默认上游 %s", addr, backend)
	for service, target := range upstreams {
		log.Printf("[网关]   %-8s → %s", service, target)
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("网关启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("[网关] 收到退出信号，开始优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[网关] 优雅关闭失败: %v", err)
	}
	log.Println("[网关] 已退出")
}

// accessLog 打印一行「谁访问了什么、结果如何、trace 是多少」。
//
// 它就是「trace id 有什么用」的现场演示：出了问题时，拿着用户看到的那个编号，
// 在日志里搜一下，就能定位到具体是哪一次请求。
func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Printf("[网关] %s %s -> %d %s trace=%s",
			c.Request.Method,
			c.Request.URL.Path,
			c.Writer.Status(),
			time.Since(start).Round(time.Millisecond),
			trace.FromGin(c),
		)
	}
}

// envOr 读环境变量，没有（或只有空格）就用默认值。
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
