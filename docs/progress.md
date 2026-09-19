# 项目进度记录

每次重新开启项目时，先读这里并向用户汇报“上次完成 / 下次计划”。完成一个模块后更新对应状态。

## 当前状态

- 当前阶段：阶段 6（进行中，**6.1 冲 1 万 QPS、6.2 演示控制台都已完成**，剩下 6.3）
- 更早一轮完成：**优化记录 7** —— ① Gin 访问日志换成「只记异常」（状态码 >= 400 或耗时 >= 500ms 才写）；② `r.Run()` 换成显式 `http.Server` 并补读写/空闲超时 + 信号优雅退出；③ 开 Release 模式；④ 业务拒绝改成错误哨兵、handler 映射 400/404/409/429；⑤ Kafka 生产者 `BatchTimeout` 10ms → 1ms；⑥ Redis 连接池 `PoolSize=512`。另外修掉一处**正确性缺陷**：对账任务在「有订单在途」时会拿偏小的 `sold` 反推库存、把已卖出的名额加回 Redis（实测多放出 10,634 个名额）；修法是给队列加「在途订单计数 + 每个消费者是否追平过」两个信号，两者都满足才允许覆盖 Redis
- 上次完成：**优化记录 8（阶段 6.1）：把抢购链路合并成「一次 Redis 往返」，1 万 QPS 目标达成**
  - 代码改动：`internal/seckill/cache.go` 新增 `hotPathLuaScript`——把「限流计数 → 读商品信息 → 校验活动时间窗 → 幂等判重 → 限购判断 → 扣库存」全部塞进同一个脚本，并把商品信息直接 `return` 回来；新增热路径入口 `TrySeckill` 和冷路径 `WarmUp`；**删掉** `PreDeduct` / `GetItem` / `SetItem` / `GetActivity` / `SetActivity` 五个各自代表一次往返的旧方法。`internal/seckill/service.go` 的 `Seckill` 改成「先打一次热路径；脚本回 `-5`（缓存没准备好）就回源 MySQL 填缓存后重试一次」；限流改成**按用户** 100 次/秒并搬进脚本（不再额外占一次往返），新增 `SetRateLimitObserver` 上报指标。`internal/seckill/router.go` 把抢购接口和管理类接口拆成两个路由组。`internal/seckill/handler.go` 新增 `ErrRateLimited → 429`。新增 `cmd/server/pprof.go`：设 `ENABLE_PPROF=1` 才在 `127.0.0.1:6060` 开分析端口（默认关闭、只绑本机）
  - 性能验证（同一台机器、同一份 k6 脚本做 A/B，HOLD=10s、商品 id=17）：64/128/256/512/1024 VU 从 5,600 / 6,266 / 6,814 / 6,668 / 6,843 QPS 提到 10,464 / 14,005 / **16,096** / 14,855 / 15,212 QPS，提升 **1.87× ~ 2.36×**；512 VU 下 p50 从 84.7ms 降到 **37.1ms**（p95 56.7ms、p99 71.8ms）；五档全部 0 错误、0 限流。**峰值约 16,000 QPS，是 1 万目标的 1.6 倍**
  - 状态码回归：200 正常 / 409 重复 request_id / 409 超限购 / 400 quantity=0 / 404 商品不存在（顺便证明「缓存未命中回源再重试」这条冷路径正常）/ 401 无令牌 / 409 活动已结束
  - 一致性复核：`sold` = 订单数 = 去重用户数 = 去重 `request_id` 数 = `SUM(quantity)` = **2,183,529**，Redis 剩余 **2,816,471** = 5,000,000 − 2,183,529；Kafka 6 个分区 LAG 全 0；日志 0 panic、0 补偿失败
  - 瓶颈定位（本轮重点结论）：用 pprof 采 20 秒 CPU profile（512 VU），**45.19% 在 `runtime.cgocall`**（阻塞式 socket 系统调用），其中 **42% 是 `net.(*conn).Write`**（把 HTTP 响应写回客户端），业务代码几乎不上榜；同时开两个 k6 进程总吞吐反而降到约 13,000/s，说明是**服务端已经饱和**、不是压测客户端发不动。**结论：再往上不是改代码的事，而是部署层面的事**（Linux/epoll、`GOMAXPROCS` 调参、HTTP 长连接、单机多实例 + 负载均衡）
  - 隔离基准：只打 Redis（1 次往返）约 34,000/s；Redis + Kafka 全链路约 30,500/s；只发 Kafka 约 86,200/s —— Kafka 不是瓶颈
- 本次完成：**阶段 6.2 演示控制台：新增 `internal/demo` 包，让人一眼看见系统在干什么**（纯新增，没动后端业务逻辑）
  - `GET /demo`：单文件页面（`internal/demo/web/index.html`，用 `go:embed` 打进二进制，原生 JS，不需要 npm、不需要打包工具），深色主题，五个区块：一键准备 / 登录注册 / 独立秒杀页 / 实时观测 / 现场演示步骤
  - `GET /demo/data`：只读接口，把 MySQL 的 `total_stock`、`sold`，Redis 的 `remaining_stock`，Kafka 在途订单数 `pending` 拼成一份 JSON，页面每秒拉一次；`remaining_stock` 用指针字段，好区分「还没预热（null）」和「真的卖光了（0）」
  - `POST /demo/seed`：一键造数据——商品 → SKU → 库存 → 秒杀活动（`now-5min` ~ `now+2h`）→ 秒杀商品 → 预热 Redis → 演示账号（`demo1..demoN`，密码 `demo1234`）。幂等：重复点不会造重复数据，只有秒杀活动每次新建；`DEMO_SEED=0` 启动即可关掉（返回 403）
  - 配套改动：`seckill.Cache.Stock`（读 Redis 剩余）、`seckill.Queue.Pending`（在途订单数）、`seckill.Service.WarmUpStock`（商品信息 + 库存一起预热）、`product.FindSKUByProductAndSpec`（判断 SKU 是否已存在）；`internal/pkg/metrics/middleware.go` 把 `/demo`、`/metrics` 排除出 `totalRequests`，避免演示页每秒轮询污染压测口径
  - 实测：`gofmt -l cmd internal` / `go vet ./...` 无输出、`go build` 通过；`GET /demo` → 200 `text/html; charset=utf-8`；seed 出 50 件库存后抢 1 件，等落库追平 `sold=1`、Redis 剩余 `49`、`pending=0`；同 `request_id` 重放 → 409、同用户换 `request_id` 再抢 → 409 限购、换账号 → 200；连打 5 次 `/demo` + 5 次 `/metrics` 后 `totalRequests` 只涨了 2（那 2 次是 `/health`）；`DEMO_SEED=0` 时 seed 返回 403 而 `/demo` 仍 200
  - 指标口径提醒：`seckillRequests` 是**入口口径**，`seckillSuccess / seckillDuplicate / seckillFailure` 是**消费端落库口径**（`internal/seckill/worker.go` 上报），两者差值就是还在 Kafka 里排队的订单，页面文案已按这个口径写死
- 上一轮文档产出：`docs/benchmark.md` 新增「优化记录 8」（改动表、返回值约定、三个取舍、A/B 对比、CPU profile 结论、一致性复核、已知小风险）；`docs/architecture.md` 更新秒杀时序图（4 次往返 → 1 次 Lua）、三层防线、Redis Key 表（新增 `:window`、限流 key 改按用户）、pprof 开关、第 10 节瓶颈结论；`docs/interview.md` 新增「第 ⑤ 轮优化故事」、数据手册补新数据、重写 Q6（QPS 怎么冲上去的）和 Q8（限流为什么搬进 Lua）、更新不足清单；`docs/build.md` 的 6.1 标记完成并补实测结论
- 下一步计划：做 **6.3 AI 辅助可观测**（指标基线告警、压测结果自动总结、自适应限流，贴着主线按性价比做多少算多少）。**明确不做 LLM 导购聊天**，理由写在 `docs/build.md` 6.3 里
- 更早两轮的结论（备查）：优化记录 6 用 k6 对照定位出「Gin 自带的逐请求访问日志」是第一个天花板（写文件 6,500/s vs 丢弃 55,000/s，差 9 倍），并用隔离基准量出「一次抢购 4 次 Redis 往返」的上限约 9,000/s；优化记录 7 在不动链路结构的前提下把峰值做到 5,907 QPS，并修掉对账超卖隐患
## 阶段进度

- [x] 阶段 0：环境准备与项目骨架
- [x] 阶段 1：用户模块（注册/登录）
- [x] 阶段 2：商品与库存模块
- [x] 阶段 3：购物车与普通下单
- [x] 阶段 4：秒杀核心模块
- [x] 阶段 5：高并发增强
- [ ] 阶段 6：压测与优化（进行中，6.1 冲 1 万 QPS、6.2 演示控制台已完成，6.3 待做）
