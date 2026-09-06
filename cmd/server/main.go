package main

import (
	"log"
	"net/http"

	"shopping_market/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8080"
	}

	http.HandleFunc("/health", healthHandler)

	log.Printf("服务启动，监听地址: %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

// healthHandler 是健康检查接口，用来确认服务还活着。
// 浏览器或压测工具访问 /health 时，会走到这个函数。
//
// TODO(你来实现)：返回 JSON 内容 {"status":"ok"}。
// 提示：
//   1. 用 w.Header().Set("Content-Type", "application/json") 告诉客户端返回的是 JSON；
//   2. 用 w.Write([]byte(`{"status":"ok"}`)) 把内容写回客户端。
func healthHandler(w http.ResponseWriter, r *http.Request) {
}
