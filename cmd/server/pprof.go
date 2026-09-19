package main

import (
	"log"
	"net/http"
	"net/http/pprof"
	"os"
)

// enableProfilingIfRequested 在设置了环境变量 ENABLE_PPROF=1 时，
// 额外监听一个只对本机开放的端口，提供 Go 自带的性能分析接口。
//
// 为什么需要它：这个项目的重点是“高并发下到底慢在哪里”。
// 猜瓶颈（觉得是 Redis？觉得是 Kafka？）经常猜错，实测最靠谱。
// 打开之后可以用这些地址拿到真实的运行数据：
//
//	go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30   // CPU 火焰图
//	go tool pprof http://127.0.0.1:6060/debug/pprof/heap               // 内存占用
//	curl http://127.0.0.1:6060/debug/pprof/goroutine?debug=1           // 看有多少 goroutine 卡在哪
//
// 为什么默认不开、而且只监听 127.0.0.1：
// 这些接口会暴露内存和调用栈，属于“调试后门”，线上绝对不能对外网开放。
// 所以做成开关 + 只绑本机，平时是关着的。
func enableProfilingIfRequested() {
	if os.Getenv("ENABLE_PPROF") != "1" {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	go func() {
		log.Println("性能分析接口已开启: http://127.0.0.1:6060/debug/pprof/")
		if err := http.ListenAndServe("127.0.0.1:6060", mux); err != nil {
			log.Printf("性能分析接口退出: %v", err)
		}
	}()
}
