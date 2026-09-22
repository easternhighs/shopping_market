# 项目进度记录

每次重新开启项目时，先读这里并向用户汇报“上次完成 / 下次计划”。完成一个模块后更新对应状态。

## 当前状态

- 当前阶段：**阶段 13 已完成**（网关收口）。微服务设计讨论稿已写完，网关与 trace 的骨架已落地（`internal/gateway` + `internal/pkg/trace` + `cmd/gateway` + `make run-gateway`），**6 处空已填完**，并修掉 5 处会让网关「整体不可用」的缺陷，六步验证流程全部走通。`gofmt` / `go vet` / `go build` 干净
- 本次完成：**阶段 13 收口：填完 6 处空，并修掉 5 处让网关「整体不可用」的缺陷，六步验证全部走通**（阶段 13 已完成）
  - 先说结论：六个空填完之后，网关照旧是**每个请求都失败**。原因是那 6 个空只是「业务逻辑」，而网关能不能跑起来还取决于一些**框架约定**——这些约定不在挖空的范围内，但错了整个网关就瘫痪。这一轮一共查出并修掉 5 个缺陷，其中缺陷 A 是最要命的，也是你这次指出的那一个
  - **缺陷 A（关键）：`Director` 和 `Rewrite` 只能有一个。**`httputil.NewSingleHostReverseProxy()` 会**默认帮我们填一个 `Director`**；而按官方推荐改用 `Rewrite`（`Director` 已被标记「将来会被 `Rewrite` 取代」）之后，两个同时存在，`net/http/httputil` 直接拒绝工作，报：`ReverseProxy must have exactly one of Director or Rewrite set`。表现是**所有**请求都回 502「上游服务不可用」，但后端其实活得好好的——非常有误导性。修法是在 `internal/gateway/proxy.go` 里显式写一行 `proxy.Director = nil`（保留用户写的 `Rewrite` 写法）
  - 缺陷 B：`IsPublic` 用 `slices.Contains` 做**全等**比较。白名单里写的是 `/shop`，而演示前台实际请求的是 `/shop/data`、`/shop/seckill`……全等的结果是只有一字不差的 `/shop` 能过，其它全是 401，演示前台和控制台整片打不开。改成 `strings.HasPrefix` 做**前缀**匹配（「先查私有名单、再查公开名单」的顺序保持不变）
  - 缺陷 B2：`/healthz` 既不在 `Routes` 也不在 `PublicPaths`，按「默认要登录」的规则被拦成 401，验证流程第 1 步直接卡住。已把 `/healthz` 加进 `PublicPaths`
  - 缺陷 C：`trace.Middleware` 里先写了个 `if` 判断请求头有没有编号，紧接着**又无条件** `id = NewID()`，那个 `if` 等于白写——上游传进来的编号每次都被丢掉，每经过一跳就换一次编号，链路追踪直接断掉。改成 `if id == "" { id = NewID() }`（有就沿用、没有才生成）
  - 缺陷 D：`MatchRoute` 里把**请求的完整路径**存进了 `longerRoute.Prefix`（而不是路由自己的前缀），于是下一次比较退化成 `len(path) > len(path)`、永远为 false，「最长前缀匹配」名存实亡，实际变成「第一条命中的赢」。改成比较 `route.Prefix` 的长度、命中就整条赋值
  - 缺陷 E1：`WithID(c, id)` 传的是 Gin 上下文。`*gin.Context` 碰巧也实现了 `context.Context` 接口所以**能编译过**，但请求真正携带的 context 是 `c.Request.Context()`，语义上应该用后者
  - 缺陷 E2：鉴权失败原本用 `c.AbortWithStatus(401)`，返回空 body。和项目里其它地方（`internal/user/auth.go`）的风格不一致，改成 `c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})`
  - **顺带堵掉一个安全漏洞**：网关在公开路径上是**直接放行**的，如果不主动清掉客户端的 `X-User-Id`，客户端就能自己伪造一个 `X-User-Id: 15` 原样转发给下游。现在下游还不看这个头所以没出事，等阶段 14 拆完 user 服务、下游改成「无条件相信网关的头」，这就是一个能直接冒充别人的洞。已在 `Auth()` 的第 0 步加 `c.Request.Header.Del(HeaderUserID)`
  - 另一个刻意的决定：**不调用 `req.SetXForwarded()`**。它会把 `X-Forwarded-For` 设成「客户端传来的值 + 真实 IP」，而下游取客户端 IP 用的是 `c.ClientIP()`（读 XFF 的第一段），客户端只要每次换一个 XFF 就能每次换一个限流桶、把限流绕过去。将来真要加，正确做法是「覆盖」而不是「追加」
  - 实测（`server_s13.exe` :8080 + `gateway_s13.exe` :8081）：`/healthz` → **200**；未带 token 访问 `/me` → **401** 且是规范 JSON；`POST /login`（`demo1`/`demo1234`）→ 200 拿到顶层 `token`；带 token 走 `/me`、`/cart/items` → **200**；`/shop/data`、`/shop/seckill` → **200**（缺陷 B 的回归点）；`/shop/my` 不带 token → 401、带 token → 200（证明「先私有后公开」的顺序是对的）；带 `X-Trace-Id: my-fixed-id-123` 请求 `/healthz`，**响应头原样回同一个值**（缺陷 C 的回归点）；`GATEWAY_BACKEND=http://127.0.0.1:19999` → **502**，响应头和日志里都带同一条 trace id
  - 额外验证（用一个临时的回显服务当上游）：确认网关**确实把 `X-Trace-Id` 转发给了下游**、客户端伪造的 `X-User-Id` **被摘掉**、`Path` **没有被改写**（`/shop/data` 原样到达上游）
  - 说明：trace id 目前只是**从网关发给了下游**，下游（现在这个单体）还没接 `trace.Middleware`、日志里也还没打这个编号。这是阶段 14 拆服务时要补的（见 `docs/microservices.md` 的「trace id 全程透传」）
- 上一轮完成：**阶段 13 起步：写完微服务拆分讨论稿，并搭好「网关 + trace id」骨架（留 6 处空）**（阶段 13 进行中）
  - 要解决的问题：前 12 个阶段功能和压测都做完了，但有一问一直没答上——「万一要拆微服务，你从哪儿下手？」。这一轮先把**设计想清楚**（写成文档），再把微服务的**第一块砖（网关）**立起来：网关负责四件事——哪个路径归哪个服务、谁需要登录、这次请求的编号是什么、请求转给谁。这四件事想明白了，以后把某个模块搬成独立进程就只是「换个上游地址」
  - 文档产出：新增 `docs/microservices.md`（272 行，讨论稿，不写代码只写决策）。内容包括：① 现状体检——9 个业务包、跨包依赖只有 4 条（`order→inventory`、`payment→order`、`shop→product,seckill`、`demo→user,product,inventory,seckill`）；② **关键发现：秒杀自带一套库存池**（`seckill_items.total_stock/sold` 独立于 `stocks` 表，秒杀链路全程不碰 `inventory.stocks`），所以秒杀模块数据自治、是最好拆的一块；③ 三处硬耦合（`internal/order/service.go:67` 那个共用的 `DeductWithDB(tx,...)`、`internal/demo/handler.go:76` 的 `/demo/seed` 一次写四类表、`internal/shop` 在服务端拼 product+seckill）；④ 目标拓扑（gateway / user / product+inventory / order+cart+payment / seckill 五块，各自独占哪些表）；⑤ **拆分必须补的 5 件事**（鉴权下沉到网关、秒杀热路径一跳都不能加、下单扣库存改成「RPC + 本地消息表 + 重试」并明确否决 2PC/TCC/Seata、幂等与跨服务对账、trace id 全程透传）；⑥ 阶段 13~17 的落地路线；⑦ 代价对照表（**特别写明：同机跑多进程会互抢 CPU，微服务 QPS 通常反而比单体低，要证明收益必须跑多机**）
  - 代码骨架（都是新文件，**没有动任何现有业务代码**）：
    - `internal/pkg/trace/trace.go`：请求编号工具。header 名 `X-Trace-Id`、`WithID` / `FromContext` / `FromGin` 已写好，**留 2 个空**（`NewID`、`Middleware`）
    - `internal/gateway/doc.go`：包注释，讲清网关管哪四件事、为什么做成「旁路式」
    - `internal/gateway/route.go`：路由表（12 条路径前缀 → 6 个服务 `user`/`product`/`order`/`seckill`/`shop`/`console`）、公开白名单 `PublicPaths`、藏在公开前缀下的私有名单 `ProtectedPaths`（`/shop/my`）、读 `GATEWAY_UPSTREAM_<服务名大写>` 的 `ServiceUpstream`；**留 2 个空**（`MatchRoute` 最长前缀匹配、`IsPublic` 判断顺序）
    - `internal/gateway/auth.go`：网关统一验 JWT，验过就把用户 id 塞进 `X-User-Id` 请求头给下游（下游信任这个头，所以下游**不能暴露公网**）；**留 1 个空**（`Auth` 五步）
    - `internal/gateway/proxy.go`：反向代理。`hopByHopHeaders`、`Proxy` 结构、`NewProxy`（一个服务复用一个 `ReverseProxy`，复用连接池）、`Handler()`（匹配路由 → 转发，未命中 404、上游挂了 502）、`ErrorHandler`（502 + trace id + 一行日志）都已写好；**留 1 个空**（`proxy.Director` 改写请求四步）
    - `cmd/gateway/main.go`：网关入口，与 `cmd/server/main.go` 同款结构（`envOr` 读环境变量、`gin.New()+Recovery`、中间件顺序 **trace → accessLog → Auth**、自带 `GET /healthz` 返回服务/上游映射、防御性超时、优雅退出）。环境变量：`GATEWAY_ADDR`（默认 `:8081`）、`GATEWAY_BACKEND`（默认 `http://127.0.0.1:8080`）、`GATEWAY_UPSTREAM_<服务名大写>`
    - `Makefile`：新增 `make run-gateway` / `make build-gateway`
  - 为什么做成「旁路」：网关是**新端口 8081**、转发给现在这个单体 8080，`cmd/server/main.go` 一个字都不改。这样零回归风险（原来怎么压测还怎么压测）、出问题不启动网关就回退了、还能拿「直连 8080 vs 走 8081」做一次干净的性能对照
  - 骨架实测（已跑通，证明脚手架可用）：`gofmt -l cmd internal` 无输出、`go vet ./...` / `go build ./...` 通过；网关起来后 `GET /healthz` 返回 **200**，打出「监听 :8081，默认上游 http://127.0.0.1:8080」和 6 行服务映射；`GET /shop`、`POST /login` 返回 **404「网关没有为这个路径配置上游服务」**（空 3 还没填的表现）、`trace_id` 是空串且响应头没有 `X-Trace-Id`（空 1 + 空 2 还没填的表现）——**这些都是「挖空」的正常状态，不是 bug**
  - 明确没做的事：不动 `cmd/server/main.go`（避免污染压测过的热路径）、不拆任何业务模块（那是阶段 14 以后的事）、不引入服务发现 / 配置中心
- 上一轮完成：**阶段 12 收口：把 5 个前端函数和 1 处越权校验全部填完，普通下单链路现场可点、越权被挡住**（阶段 12 完成）
  - 要解决的问题：上一轮把页面骨架和「空」都摆好了，但结算按钮点了没反应、`demo2` 还能改删 `demo1` 的购物车明细。这两件事不解决，阶段 12 就只是一张假页面
  - 填完 5 个前端函数：`cart.html` 的 `findSKU(skuID)`（拿 SKU 号在 `GET /shop/data` 的目录里两层循环查表，命中返回 `{product, sku}`、否则 `null`）、`cartTotal()`（`reduce` 把每行「单价 × 数量」累加成合计，查不到的 SKU 按 0 算）、`placeOrder()`（拼 `request_id = "cart-" + 时间戳`，把购物车明细映射成 `[{sku_id, quantity}]` 调 `POST /orders`，成功后把订单对象存进 `orderResult` 并切到订单详情）、`payOrder(orderID)`（调 `POST /orders/:id/pay`，成功后把订单状态改成 `paid` 并重绘）；`index.html` 的 `addToCart(skuID)`（未登录 / 数量小于 1 先拦，否则 `POST /cart/items`）
  - 填完越权校验（IDOR）：`internal/cart/service.go` 的 `UpdateQuantity` / `RemoveItem` 先调仓储层的 `FindItemByIDForUser(itemID, userID)` —— 这条 SQL 把 `user_id` 写进 `WHERE`，查不到就返回 `ErrItemNotOwned`，Service 直接返回、不再往下执行改 / 删；`internal/cart/handler.go` 把 `ErrItemNotOwned` 映射成 **404「购物车明细不存在」**
  - 为什么回 404 而不是 403：403 等于告诉攻击者「这条明细真实存在，只是不属于你」，白送一个「哪些 id 有效」的探测接口；统一回 404，让「不存在」和「不是你的」长得一模一样
  - 顺手清掉练习脚手架：`cart.html` 里的 `BLANKS` 数组、`renderBlanks()` 及其调用、两处「xxx() 还没实现」的兜底提示全部删除，页底改成「结算链路的核心四步」说明；全库 `TODO(你来实现)` **0 命中**
  - 实测（`.tmp/server_cart.exe` + `SECKILL_PAY_TIMEOUT=30s`，:8080）：`gofmt -l cmd internal` 无输出、`go vet ./...` / `go build ./...` 通过；三个页面的内联脚本过 node 语法检查；用 DOM 桩直接打真实接口 —— `findSKU(16)` 命中「演示充电宝 · 随行 20000 / 20000mAh / ¥149」、`findSKU(999999)` 返回 `null`、`cartTotal()` = 149；点结算真实拿到自增订单 `#12` 并切到详情、点支付后状态变 `paid`；加购未登录提示「请先登录」、数量 0 提示「数量必须大于 0」、同一 SKU 连加两次合并成一条 `quantity=2`
  - 越权实测：`demo2` 对 `demo1` 的明细 id=3 做 `PATCH`（qty=777）/ `DELETE` 全部 404「购物车明细不存在」，`PATCH` 一个不存在的 id=999999 也是同样的 404；`demo1` 改自己的明细仍然 200。`demo2` 购物车明细数为 0，`demo1` 的 `id=3 / sku=16 / qty=1` 原样未动
  - 其他复核：重复支付第二遍返回 400「订单状态不允许该操作」；`GET /shop/cart` 返回 200、23934 字节、`TODO` 计数 0；秒杀链路一个字没动（Redis 脚本、Kafka、压测过的热路径都没碰），阶段 6 的 1 万 QPS 结论依旧成立
  - 已知简化（同步写进了 `docs/build.md`）：① 下单成功后购物车明细不会自动清空，要手动删，是演示用的简化；② 订单侧的越权（`GET /orders/:id`、`pay`、`cancel` 只认订单号、不校验这单是不是你的）本轮只在文档里说明、没有改代码，避免范围蔓延；③ `PATCH` 传 `quantity <= 0` 会走数据库报错返回 500 而不是 400，是阶段 3 就留下的瑕疵
- 上一轮完成：**阶段 12 骨架：把「普通下单」这条链路也变成能点的页面**（骨架已就绪，空留到本轮填完）
  - 要解决的问题：前 11 个阶段把秒杀链路做到了 1 万 QPS，但面试官几乎一定会追问「那普通下单你们怎么做？」。阶段 3 的购物车 / 订单接口一直都在，只是没有页面能点，现场只能靠 curl，讲起来很干。这一章补前台页面，让一次演示里能并排看到**两条下单链路**：常规链路（同步事务、强一致、直接扣 MySQL 库存）vs 秒杀链路（Redis 预扣 + Kafka 异步落库、最终一致）
  - 新增 `internal/shop/web/cart.html`（购物车 + 结算页）：明细表（商品 / 规格 / 单价 / 数量 / 小计 / 操作）、改数量、删除、合计、可展开的结算面板（收货信息仅本地校验、不发给后端）、提交订单、模拟支付、订单详情回显；页底两块面试话术 ——「留给你实现的部分」清单（本轮已移除）、同步事务 vs 异步最终一致对比
  - `internal/shop/web/index.html` 商品列表页：导航加「购物车」入口，商品行新增「购物车」列（数量框 + 加入按钮）；给每秒一次的自动刷新加 `isEditing()` 判断，避免重绘冲掉正在输入的数字
  - 三个页面（`/shop`、`/shop/seckill`、`/shop/cart`）导航互通，登录态共用同一个 `localStorage` 键。页面本身公开，私有数据全靠接口上的 JWT 中间件把守
  - 后端只做最小改动：`internal/shop/handler.go` 加 `//go:embed web/cart.html` + `CartPage`；`internal/shop/router.go` 注册 `GET /shop/cart`。页面调的接口全是阶段 3 就有的 —— 打开购物车 `GET /cart/items`、加购 `POST /cart/items`、改数量 `PATCH /cart/items/:item_id`、删除 `DELETE /cart/items/:item_id`、下单 `POST /orders`、支付 `POST /orders/:id/pay`，商品名 / 规格 / 单价来自 `GET /shop/data` 前端查表
  - 留出的空（5 个）：`cart.html` 的 `findSKU()`、`cartTotal()`、`placeOrder()`、`payOrder()`，`index.html` 的 `addToCart()`；函数体里只有注释 + `TODO(你来实现)`（本轮已全部填完，见上）
  - **第 6 个空是一道安全题**：实测发现 `demo2` 能改 / 删 `demo1` 的购物车明细，也能读别人的订单，是典型的越权（IDOR）。仓储层补上 `FindItemByIDForUser(itemID, userID)`，`internal/cart/service.go` 的 `UpdateQuantity` / `RemoveItem` 改成接收 `userID`，**校验逻辑当时留空**（本轮已填完）。订单侧同类问题只在文档里说明、不动代码（避免范围蔓延）
  - 已实测：`gofmt -l cmd internal` 干净，`go vet ./...`、`go build ./...` 通过；三个页面的内联脚本过 node 语法检查；`GET /shop/cart` 返回 200 并检出 5 处 `TODO(你来实现)`；接口链路实跑通（加购 sku16×2 → 改 3 → `POST /orders` 拿到真实自增订单号 → 支付变 `paid`）；越权复现成功（`demo2` 改 `demo1` 的明细返回「已更新」；本轮填完校验后实测已变成 404，见上）
  - 顺带说明：秒杀链路一个字都没动（不碰 Redis 脚本、不碰 Kafka、不碰任何压测过的热路径），所以阶段 6 的压测结论依旧成立；`orders` 表没有金额列是阶段 3 的已知简化，页面上的合计是前端按 `GET /shop/data` 的价格算的
- 上一轮完成：**阶段 11 面试演练与资料收口：把「代码写完了」变成「现场讲得出来、换台机器也跑得起来」**
  - 要解决的问题：演练材料里有两处会在面试现场直接炸掉 —— ① 演示页生成的压测命令指向 `.tmp/k6_bench.js`，而 `.tmp/` 不进版本库，换台机器**必然报「找不到脚本」**；② 附录 B 的双屏脚本讲到「已抢到（订单 #xxx）」就转去讲限购了，**完全没覆盖阶段 9 的「抢到不付款 → 超时关单 → 库存回补」**，而那正是本项目第二个核心亮点
  - 脚本搬家：`.tmp/k6_bench.js` → `scripts/k6_bench.js`、`.tmp/k6_slide.js` → `scripts/k6_slide.js`（都进版本库）；压测结果 `--summary-export .tmp/k6_summary.json` 仍留在 `.tmp/`（那是产物，不是源码）。同步改掉演示页第 ⑥ 块的命令生成、`docs/build.md`、`docs/progress.md`、`cmd/benchreport` 注释里的路径引用
  - 附录 A（单屏）新增第 7 步「限流本身也演进过」：固定窗口的边界突发（半秒内放过接近 2 倍）→ 滑动窗口记放行时刻，附 A/B 数字（合计 200 vs 100）和复现命令 `k6 run -e ITEM=<商品ID> scripts/k6_slide.js`
  - 附录 B（双屏）新增第 7 步**超时关单**整段：抢到 → 付款倒计时 → 倒计时走完 → 左屏翻成「⌛ 超时未付款，订单已自动取消」且剩余库存涨回去、右屏 `sold` 减回去、服务日志出现「秒杀超时处理（延时队列）本轮 1 笔」；这 30 秒的等待正好用来讲滑动窗口那个故事，不用冷场。演示前清单同时补上两个最容易被问住的点：**先设 `$env:SECKILL_PAY_TIMEOUT="30s"` 再起服务**、**`/demo/seed` 不传 `total_stock` 才是 30/80/200/500 四档**
  - 顺手修掉文档缺陷：附录 A 末尾重复的一整段、附录 B 造数据步骤「点按钮 / 调接口」写混、「万一现场出状况」补两条（倒计时不翻牌、库存数字不动）
  - 实测（`SECKILL_PAY_TIMEOUT=30s`，用全新 seed 出来的 item 33、库存 30）：点抢购 → 接口返回 `id=0`/`status=queued`、Redis 剩余 30→29；**+1 秒**落库成订单 `#4423816`、`sold` 变 1；**+31 秒** `status=cancelled`、Redis 剩余回到 30、`sold` 回到 0，日志打出「秒杀超时处理（延时队列）本轮 1 笔：关单 1 笔、回补库存 1 笔、失败 0 笔」。再用 item 34 复核一遍「落库后 `sold` 0→1、关单后回到 0、剩余 80→79→80」，并确认「关单之后同一个用户可以重新抢（200）、待付款期间换个 `request_id` 再抢会被限购挡下（409「超出每人购买限制」）」——这也是附录 B 把「限购」排在第 6 步、「超时关单」排在第 7 步的原因
  - 顺带修好本文件一处孤儿条目：阶段 9 的几条子条目原来挂在阶段 10 下面，缺一个顶层条目，已补上「再上一轮完成：阶段 9」
  - 编译与静态检查：`gofmt -l cmd internal` 无输出、`go vet ./...`、`go build ./...` 全部通过；`k6 inspect` 能正常解析 `scripts/` 下的两个脚本
- 上一轮完成：**阶段 10 滑动窗口限流：把「窗口边界上放过的 2 倍流量」堵上**
  - 要解决的问题：抢购接口的限流原本是**固定窗口**——key 是 `ratelimit:seckill:user:<用户ID>:<窗口号>`，窗口号 = `当前毫秒 / 1000`，等于把时间切成一个个「自然秒」。同一个用户在第 1 秒的最后 10 毫秒打 100 次、第 2 秒的头 10 毫秒再打 100 次，两次落在**不同的窗口**里，各自都「没超 100」，于是这 20 毫秒内实际放过了 200 次——「1 秒最多 100 次」的承诺被放大了接近 2 倍。这类攻击叫「**边界突发**」，攻击者不用破解限流，掐着秒表在整秒前后各打一发就行，窗口切得越粗洞越大
  - 修法（一句话：**别数数，改成记时刻**）：用 Redis 有序集合 ZSET 当窗口，每放行一次就 `ZADD <限流 key> <放行时刻毫秒> <成员>`；判断时先 `ZREMRANGEBYSCORE` 把滑出窗口的旧记录删掉，再 `ZCARD` 数剩下几条，`>= 100` 就拒绝。判断依据从「现在是第几个固定窗口」变成「**最近 1000 毫秒内到底放行过几次**」，没有窗口翻页，也就没有那个边界瞬间。删旧与数数在同一段 Lua 里原子执行，并发下也不会算错
  - 三个容易写错的细节：① **只有「放行」的请求才记进窗口**，被拒的也记的话窗口会被自己的拒绝记录越填越满，变成「越限封得越久」（另一个 bug）；② **成员不能只用毫秒时间戳**，同一毫秒的并发请求会互相覆盖导致计数偏少、限流放水——热路径用 `request_id`（天然唯一，且同一请求重放不多占配额），通用限流器用「毫秒-进程内自增序号」；③ **`PEXPIRE` 只是兜底**，判断不依赖过期，它和 `ZADD` 同在一段 Lua 里原子执行，顺带把老实现「`INCR` 成功但进程在 `EXPIRE` 前挂掉、key 永不过期」的隐患一并消掉了
  - key 随之简化：`ratelimit:seckill:user:<用户ID>:<窗口号>` → `ratelimit:seckill:user:<用户ID>`；类型从「整数计数」变成「ZSET 记时刻」；有效期从「1 个窗口」变成「最后一次放行之后再 1 个窗口」
  - 两处限流的取舍**故意不同**：抢购热路径（`internal/seckill/cache.go` 的 `hotPathLuaScript`）把滑动窗口逻辑直接写进那段大脚本的**第 1 步**，和读缓存、校验活动、判重、限购、预扣共用**同一次 Redis 往返**；秒杀管理接口用的通用限流器（`internal/pkg/ratelimit/redis_limiter.go`）本来就是独立一次往返，直接改内部实现即可，`Limiter` 接口与 `middleware.go` 完全没动。**没有让热路径复用它**——复用意味着先问一次「能不能过」再执行大脚本，多一次 RTT，正好把 6.1 的优化还回去；宁可两份长得像的代码，也不牺牲热路径的往返次数
  - 实测（A/B 对照：用 `git archive HEAD` 把改动前的代码单独编译成一个二进制，两边跑同一份脚本）：同一个用户、两批各 110 个请求（`request_id` 各不相同），第一批卡在某自然秒的 500ms 处、第二批卡在下个自然秒的 60ms 处

    | 版本 | A 批放行 | B 批放行 | 合计放行 | 两批间隔 |
    | --- | --- | --- | --- | --- |
    | 固定窗口 | 100 | 100 | **200** | 305ms |
    | 固定窗口 | 100 | 100 | **200** | 282ms |
    | 固定窗口 | 94 | 100 | **194** | 213ms |
    | 滑动窗口 | 100 | 0 | **100** | 501ms |
    | 滑动窗口 | 100 | 0 | **100** | 371ms |

    固定窗口在半秒内放过了接近 2 倍的量；滑动窗口稳定卡在 100，且 A 批正好放满、B 批全拒，正是预期形状
  - 回归验证：管理类接口的通用限流器改完后依然准确——50 个并发请求全过（无 429 误报），150 个并发请求正好放行 100、拒绝 50
  - 编译与静态检查：`gofmt -l cmd internal` 无输出，`go vet ./...`、`go build ./...` 全部通过
  - 顺带说明为什么不用令牌桶：令牌桶更好（允许一定程度的突发、还能平滑），但要额外维护「上次补充令牌的时刻 + 当前令牌数」两个状态，且在 Lua 里要处理时间回退等边界；本项目更看重「能一眼看懂、能对着时间轴讲清楚」，滑动窗口在这一点上更划算。两者都消除了固定窗口的边界洞，这是这个阶段真正要解决的问题- 本次完成：**阶段 9 订单超时关单与库存回补：抢到了不付钱，库存得能自己回来**
- 再上一轮完成：**阶段 9 订单超时关单与库存回补：抢到不付款，库存自己回来**
  - 要解决的问题：在此之前订单落库后状态就永远停在 `pending`。用户抢到名额却不付款，这个名额就**永久消失**（Redis 的剩余库存已经被扣掉，MySQL 的 `sold` 也还算着它）。这是演示和面试里最容易被追问的一句话：「抢了不付钱怎么办？」
  - 设计决定 1：**用延时队列（Redis ZSET），不用「定时轮询订单表」**。下单落库成功时 `ZADD seckill:orders:expire <到期时间戳> <request_id>`，后台每 5 秒 `ZRANGEBYSCORE ... LIMIT 200` 只取一小批已到期的订单来处理。相比「每 N 秒 `SELECT ... WHERE status='pending' AND created_at < now-N`」：① 每轮要扫的量只跟「到期订单数」有关，跟订单总量无关；② 到期时间精确到秒，不用为了不漏单把扫描窗口开大；③ 仍然保留一个兜底扫描，不把 Redis 当成唯一可靠的记录
  - 设计决定 2：**兜底扫描必须有时间窗，而且必须有索引**。ZSET 只是「加速器」——万一 Redis 没写进去或者数据丢了，订单就永远关不掉，所以每轮还要补一次 `ListExpiredPendingOrders`（`status='pending'` 且 `created_at < now-timeout` 且 `created_at > now-6h`）。这里踩了两个真实教训：① 库里躺着 440 万条压测遗留的 pending 订单，不加 6 小时时间窗的话这个扫描要跑几十小时、日志被刷爆；② 最初没有 `(status, created_at)` 索引，这条 SQL 的执行计划是 `type: index` 走主键全表扫，一次 1.6~1.8 秒，日志里满屏 SLOW SQL；补上索引、并把排序从 `id asc` 改成 `created_at asc, id asc` 之后变成 `type: range`、`rows: 1`
  - 设计决定 3（**最该记住的一条，面试也最常被追问**）：**先改 MySQL，再改 Redis，顺序不能反**。完整的关单语义是「订单置为 `cancelled` → `sold` 减回去 → Redis 库存加回去 → 用户已购件数减回去」。如果先加 Redis 再加 MySQL，一旦 MySQL 那步失败（超时、连接断），Redis 就已经多出了名额、而订单还是 `pending` —— 这是真正的**超卖**；按现在的顺序失败，最坏只是「少卖」（名额暂时消失），可以靠下一轮重试和对账补回来。**宁可少卖，不可超卖**
  - 设计决定 4：**两层幂等，缺一不可**。MySQL 侧用条件 UPDATE（`WHERE id=? AND status='pending'`，`RowsAffected==1` 才算关单成功）；Redis 侧用 `SETNX seckill:order:<request_id>:restored` 抢一个回补标记，抢到才允许 `INCRBY`。只有 MySQL 那层是不够的，只有 Redis 那层也是不够的——考虑「关单 → 用户重新抢到同一个商品 → 定时器又重试到这张旧单」：这时已购件数已经被新订单加回去了，光看已购件数会误判成「已经还过了」而**少还**；反过来若不加标记直接重试，就会**重复加库存**。两层各挡一类错误
  - 连带修正 1：**`sold` 的语义明确成「还没被取消的订单所占用的库存」**（含 `pending`，不含 `cancelled`）。关单时必须把 `sold` 一起减掉，否则对账任务会拿 `sold` 去反推 Redis 库存，把刚刚还回去的名额又吞掉
  - 连带修正 2：**`idx_user_item` 从唯一索引降级成普通索引**。这个唯一索引原本是为了保证「一个用户对一个商品只能有一张有效订单」，但超时关单之后用户想重新抢，就会撞上「旧的 `cancelled` 订单还在」的唯一约束，导致**永远抢不到**。改成普通索引后，限购交给业务逻辑（只看 `status IN ('pending','paid')`）保证。**注意 `AutoMigrate` 不会修改已存在的同名索引**，所以新增了 `internal/seckill/migrate.go`：启动时查 `information_schema.statistics`，发现还是唯一索引就 DROP 掉再建成普通索引
  - 连带修正 3：对账任务 `ReconcileItem` 增加一个判断——`inFlightExpiry` 计数器不为 0 时本轮直接跳过。它是一个**原子计数器（不是锁）**，代表「有超时关单正在处理中」。否则对账可能读到「MySQL 还没提交、Redis 已经加回」的中间状态，把库存又推回旧值。至于「Redis 库存已经扣了、但 Kafka 的消息还没落库」这个窗口，对账本来就有队列的「在途订单计数 + 是否追平过」挡着，不需要额外处理
  - 售后毒丸：`RestoreStockWithTx` 若发现商品行已经不存在（`RowsAffected==0` 且查不到该商品）就返回 `ErrItemGone`，让订单能正常关掉，而不是无限重试
  - 默认配置：`DefaultPayTimeout = 15m`，可用环境变量 `SECKILL_PAY_TIMEOUT` 覆盖（演示时设成 `30s` 就能当场看到效果）；扫描间隔 `5s`、每轮最多处理 200 笔、回补标记保留 7 天
  - 前端：秒杀独立页从「抢到了」继续往下走一格——显示「还剩 mm:ss 付款」，`pending` 到期后翻成 `expired · 订单已超时取消，库存已释放`；`/shop/data` 新增 `pay_timeout_seconds` 字段，**倒计时由服务端下发**，不在前端写死；「我的订单」页里 pending 显示成「待付款 · 还剩 xxx」
  - 实测（`SECKILL_PAY_TIMEOUT=30s`）：① **超时关单**：抢到 item 29 → `pending`、`sold` 0→1、Redis 600→599、ZSET 长度 1 → 30 秒后 `cancelled`、`sold`→0、Redis→600、ZSET 清空、已购件数→0、回补标记=1，日志一行「超时处理（延时队列）本轮 1 笔：关单 1 笔、回补库存 1 笔、失败 0 笔」；② **关单后能重抢**：同一用户换一个新的 `request_id` 再抢同一商品 → **200**，新旧两张订单共存（证明索引降级生效）；③ **已付款不会被误关**：把订单直接改成 `paid` → 到期后状态仍是 `paid`、`sold` 不变、Redis 不变、也没有回补标记
  - 前台视角复核：`/shop/my` 里 `status=pending` → 36 秒后自动变成 `cancelled`；`/shop/data` 返回 `pay_timeout_seconds=30`
  - 文档产出：`docs/build.md` 新增「阶段 9」章节（延时队列 vs 定时器、两层幂等、新手五步验证法）、`docs/architecture.md` 新增「§4.4 订单状态机与超时关单」（状态机图 + 两条路径的流程图 + Redis Key 表新增 `seckill:orders:expire` 与 `seckill:order:{id}:restored`）、`docs/interview.md` 新增 Q11「用户抢到了却一直不付款，库存怎么办？」并更新不足清单
  - 风险 / 已知取舍：① 没有接真实支付渠道，`paid` 只能靠外部写入，所以「支付成功与超时同时发生」这个竞态没法真跑；② 关单最坏延迟 = 扫描间隔 5 秒 + 处理耗时；③ 兜底扫描的 6 小时时间窗是折中，超过 6 小时还没被处理的 pending 单不会进入兜底（正常路径下延时队列早就处理了）
- 更早完成：**阶段 8 控制台压测命令生成器：压测继续交给 k6，但不再需要背命令**
  - 设计决定（本阶段的重点）：**不在服务端调用 k6**。① 压测端与被压服务抢同一台机器的 CPU，数字本身就不干净——`docs/benchmark.md` 实测两个 k6 并行时总吞吐从 16,000/s 反降到 13,000/s；② 让 Web 接口去启动外部进程，本质上是开了一个远程命令执行的口子；③ 「生成命令」与「一键执行」的便利程度差不多，但后者要额外处理子进程超时、输出回传、连点两次。**功能边界划在哪里，看收益和风险是否成正比**
  - 交付内容（只改 `internal/demo/web/index.html` 一个文件，纯前端，未动任何 Go 逻辑）：新增第 ⑥ 块「压测命令生成器」——选商品（默认跟随第 ③ 块当前选中的秒杀商品）、选模式（`seckill` / `health`）、填并发 VU（64 / 128 / 256 / 512 / 1024 快捷键）、选持续时间（10s / 30s / 60s），实时拼出完整命令行并一键复制
  - 生成的命令：`k6 run --summary-export .tmp/k6_summary.json -e MODE=seckill -e VUS=256 -e HOLD=10s -e ITEM=<商品ID> scripts/k6_bench.js`。**故意不生成 `NS`**：脚本用毫秒时间戳自动保证每轮唯一；复用同一批 `user_id` / `request_id` 会被服务端正确地判成重复请求，看起来像 Bug 其实是参数写错
  - 三个安全提示：选中商品 Redis 库存为 0 → 黄色警告（这一轮只会有「库存不足」，不会成交）；一个商品都没有 → 提示先去点第 ① 块；切到 `health` 模式 → 商品下拉框自动置灰（该模式只打 `/health`）
  - 第二条命令是 `cmd/benchreport`：把本轮 `--summary-export` 的 JSON + 服务端 `/metrics` + 告警汇成一份 Markdown 报告，压完直接能讲
  - 复制按钮做了降级：页面走 `http://`，浏览器不给用 `navigator.clipboard`，自动退回「隐藏 textarea + 选中 + `document.execCommand('copy')`」
  - 原来的第 ⑥ 块「现场怎么演示」顺延为第 ⑦ 块，里面那条写死 `<商品ID>` 的命令改成「去第 ⑥ 块复制」
  - 实测：把生成器拼出的命令原样执行（`VUS=64` / `HOLD=10s` / `ITEM=28`）→ 12 秒 **49,846** 次请求、0 连接错误、`ok=499` **恰好等于该商品当时的 Redis 剩余 499**；`go run ./cmd/benchreport -k6 .tmp/k6_summary.json -url http://127.0.0.1:8080 -out -` 正常出报告；`GET /demo` → 200 且包含新第 ⑥ 块；再用 node + 极简 DOM 桩跑页面脚本，`seckill` 生成含 `ITEM` 的命令、`health` 自动去掉 `ITEM` 并置灰商品框，无运行时报错
  - 文档产出：`docs/build.md` 新增「阶段 8」章节、`docs/architecture.md` 演示控制台接口表附近补设计说明、`docs/interview.md` 附录 B 第 5/7 步改成「用生成器拿命令」
- 前一轮完成：**阶段 7 独立前台与演示内容：让项目有一个「用户视角」的入口**
  - 定位：做的是「秒杀场景的**最小可信前台**」，不是完整电商前台——购物车/结算/支付页面不做（那部分接口已有，缺的只是页面，对高并发讲解没有增量）；但页面上的**每一个数字都来自真实接口**，没有写死的假数据
  - 新增 `internal/shop` 包（`handler.go` / `router.go` / `web/index.html` / `web/seckill.html`）：和 `demo` 包一样只做「组装」，不碰数据库表，注入的是 `product.Service` 和 `seckill.Service`；鉴权中间件当参数传入（`RegisterRoutes(r, h, auth gin.HandlerFunc)`），所以 `shop` 包不需要认识 user 模块
  - 四条新路由：`GET /shop`（商品列表页，含专场横幅/倒计时/商品卡/我的订单，`#/product/N` 看详情）、`GET /shop/seckill`（秒杀独立页，`?item=N`）、`GET /shop/data`（只读：商品+SKU+当前活动+秒杀商品含 Redis 剩余）、`GET /shop/my`（**挂 JWT**，当前用户的秒杀订单）。两个 HTML 用 `go:embed` 打进二进制
  - 秒杀独立页是一个状态机 `idle → queued → won / rejected`：点「立即抢购」按钮变「排队中」，之后每秒用 `request_id` 反查落库结果，查到就翻成「抢到了（订单 #xxx）」或「很遗憾」；页面底部有「这一单背后发生了什么」五步说明
  - 为前台补的只读能力（纯新增）：`seckill.ListUserOrders(userID)`、`product.ListSKUsByProductIDs(ids)`。后者**专门为了避免 N+1**——列表页要显示十几个商品的规格，写成「for 每个商品查一次」就是十几次查询，而这个接口每秒被拉一次
  - 演示数据从「1 个商品」升级成「4 个商品」：星海 X1（30 件/限购 1）、星河 Watch（80 件/限购 1）、声浪 Air（200 件/限购 2）、随行 20000（500 件/限购 3），同一场活动、逐个预热 Redis。库存刻意拉开档次，好同时演示「快卖光」和「还剩很多」，以及限购 2 件/3 件的规则；返回体保留旧的单商品字段，`/demo` 老代码不用改。列表页默认只列参加秒杀的商品，历史压测留下的十几条测试商品折叠在「全部商品」里
  - `internal/pkg/metrics/middleware.go` 的 `ignoredPrefixes` 从 `["/demo", "/metrics"]` 扩成 `["/demo", "/metrics", "/shop"]`；`cmd/server/main.go` 加两行装配
  - 口径提醒：不让 `/shop` 计入不只是「数字好看」——页面每秒轮询，算进去它的耗时会被算进平均延迟，而**自适应限流正是按延迟判断系统忙不忙**，口径被污染会让限流误判繁忙、主动降阈值
  - 实测：`gofmt -l cmd internal` 无输出、`go vet ./...` 通过、`go build ./...` 通过；`/shop`、`/shop/seckill`、`/shop/data` 均 200，`/shop/my` 不带令牌 → **401**；端到端 `demo1` 登录 → 抢 item 25 → 返回 `status:"queued"` / `id:0` → 0.8 秒后 `/shop/my` 出现该单且 `request_id` 一致 → 同 `request_id` 重放 → **409**；`demo2` 抢限购 2 件的商品下 2 件 → Redis 剩余 200 → **198**
  - 零超卖复核：对库存 80 的 `item=26` 用 k6 打 300 VU / 10 秒（37,615 次请求、约 3,742 req/s），**200 恰好 80 个**；MySQL `seckill_orders` 里 `item_id=26` 恰好 **80 行 / 80 件 / 80 个用户**，Redis 剩余 **0**、`sold` **80** —— 闸门与账本完全一致
  - 口径隔离复核：连打 12 次 `/shop/data`，`totalRequests` 与 `seckillRequests` 增量均为 **0**
  - 顺手记录一个现象（未深究）：那轮 k6 报告里有 68 次 `connection refused`，**全部集中在压测开始的第一秒**，怀疑是 300 个虚拟用户同时新建 TCP 连接把监听队列瞬时打满；服务端日志里一条 5xx 都没有，属于客户端建连层面的现象
  - 文档产出：`docs/build.md` 新增「阶段 7」章节（含新手启动/验证命令与新手上手五步）、`docs/architecture.md` 补分层图与「独立前台」接口清单及六个刻意设计、`docs/interview.md` 新增「附录 B：双屏演示脚本」
- 更早一轮完成：**优化记录 7** —— ① Gin 访问日志换成「只记异常」（状态码 >= 400 或耗时 >= 500ms 才写）；② `r.Run()` 换成显式 `http.Server` 并补读写/空闲超时 + 信号优雅退出；③ 开 Release 模式；④ 业务拒绝改成错误哨兵、handler 映射 400/404/409/429；⑤ Kafka 生产者 `BatchTimeout` 10ms → 1ms；⑥ Redis 连接池 `PoolSize=512`。另外修掉一处**正确性缺陷**：对账任务在「有订单在途」时会拿偏小的 `sold` 反推库存、把已卖出的名额加回 Redis（实测多放出 10,634 个名额）；修法是给队列加「在途订单计数 + 每个消费者是否追平过」两个信号，两者都满足才允许覆盖 Redis
- 上次完成：**优化记录 8（阶段 6.1）：把抢购链路合并成「一次 Redis 往返」，1 万 QPS 目标达成**
  - 代码改动：`internal/seckill/cache.go` 新增 `hotPathLuaScript`——把「限流计数 → 读商品信息 → 校验活动时间窗 → 幂等判重 → 限购判断 → 扣库存」全部塞进同一个脚本，并把商品信息直接 `return` 回来；新增热路径入口 `TrySeckill` 和冷路径 `WarmUp`；**删掉** `PreDeduct` / `GetItem` / `SetItem` / `GetActivity` / `SetActivity` 五个各自代表一次往返的旧方法。`internal/seckill/service.go` 的 `Seckill` 改成「先打一次热路径；脚本回 `-5`（缓存没准备好）就回源 MySQL 填缓存后重试一次」；限流改成**按用户** 100 次/秒并搬进脚本（不再额外占一次往返），新增 `SetRateLimitObserver` 上报指标。`internal/seckill/router.go` 把抢购接口和管理类接口拆成两个路由组。`internal/seckill/handler.go` 新增 `ErrRateLimited → 429`。新增 `cmd/server/pprof.go`：设 `ENABLE_PPROF=1` 才在 `127.0.0.1:6060` 开分析端口（默认关闭、只绑本机）
  - 性能验证（同一台机器、同一份 k6 脚本做 A/B，HOLD=10s、商品 id=17）：64/128/256/512/1024 VU 从 5,600 / 6,266 / 6,814 / 6,668 / 6,843 QPS 提到 10,464 / 14,005 / **16,096** / 14,855 / 15,212 QPS，提升 **1.87× ~ 2.36×**；512 VU 下 p50 从 84.7ms 降到 **37.1ms**（p95 56.7ms、p99 71.8ms）；五档全部 0 错误、0 限流。**峰值约 16,000 QPS，是 1 万目标的 1.6 倍**
  - 状态码回归：200 正常 / 409 重复 request_id / 409 超限购 / 400 quantity=0 / 404 商品不存在（顺便证明「缓存未命中回源再重试」这条冷路径正常）/ 401 无令牌 / 409 活动已结束
  - 一致性复核：`sold` = 订单数 = 去重用户数 = 去重 `request_id` 数 = `SUM(quantity)` = **2,183,529**，Redis 剩余 **2,816,471** = 5,000,000 − 2,183,529；Kafka 6 个分区 LAG 全 0；日志 0 panic、0 补偿失败
  - 瓶颈定位（本轮重点结论）：用 pprof 采 20 秒 CPU profile（512 VU），**45.19% 在 `runtime.cgocall`**（阻塞式 socket 系统调用），其中 **42% 是 `net.(*conn).Write`**（把 HTTP 响应写回客户端），业务代码几乎不上榜；同时开两个 k6 进程总吞吐反而降到约 13,000/s，说明是**服务端已经饱和**、不是压测客户端发不动。**结论：再往上不是改代码的事，而是部署层面的事**（Linux/epoll、`GOMAXPROCS` 调参、HTTP 长连接、单机多实例 + 负载均衡）
  - 隔离基准：只打 Redis（1 次往返）约 34,000/s；Redis + Kafka 全链路约 30,500/s；只发 Kafka 约 86,200/s —— Kafka 不是瓶颈
- 更早完成：**阶段 6.2 演示控制台：新增 `internal/demo` 包，让人一眼看见系统在干什么**（纯新增，没动后端业务逻辑）
  - `GET /demo`：单文件页面（`internal/demo/web/index.html`，用 `go:embed` 打进二进制，原生 JS，不需要 npm、不需要打包工具），深色主题，五个区块：一键准备 / 登录注册 / 独立秒杀页 / 实时观测 / 现场演示步骤
  - `GET /demo/data`：只读接口，把 MySQL 的 `total_stock`、`sold`，Redis 的 `remaining_stock`，Kafka 在途订单数 `pending` 拼成一份 JSON，页面每秒拉一次；`remaining_stock` 用指针字段，好区分「还没预热（null）」和「真的卖光了（0）」
  - `POST /demo/seed`：一键造数据——商品 → SKU → 库存 → 秒杀活动（`now-5min` ~ `now+2h`）→ 秒杀商品 → 预热 Redis → 演示账号（`demo1..demoN`，密码 `demo1234`）。幂等：重复点不会造重复数据，只有秒杀活动每次新建；`DEMO_SEED=0` 启动即可关掉（返回 403）
  - 配套改动：`seckill.Cache.Stock`（读 Redis 剩余）、`seckill.Queue.Pending`（在途订单数）、`seckill.Service.WarmUpStock`（商品信息 + 库存一起预热）、`product.FindSKUByProductAndSpec`（判断 SKU 是否已存在）；`internal/pkg/metrics/middleware.go` 把 `/demo`、`/metrics` 排除出 `totalRequests`，避免演示页每秒轮询污染压测口径
  - 实测：`gofmt -l cmd internal` / `go vet ./...` 无输出、`go build` 通过；`GET /demo` → 200 `text/html; charset=utf-8`；seed 出 50 件库存后抢 1 件，等落库追平 `sold=1`、Redis 剩余 `49`、`pending=0`；同 `request_id` 重放 → 409、同用户换 `request_id` 再抢 → 409 限购、换账号 → 200；连打 5 次 `/demo` + 5 次 `/metrics` 后 `totalRequests` 只涨了 2（那 2 次是 `/health`）；`DEMO_SEED=0` 时 seed 返回 403 而 `/demo` 仍 200
  - 指标口径提醒：`seckillRequests` 是**入口口径**，`seckillSuccess / seckillDuplicate / seckillFailure` 是**消费端落库口径**（`internal/seckill/worker.go` 上报），两者差值就是还在 Kafka 里排队的订单，页面文案已按这个口径写死
- 更早完成：**阶段 6.3 AI 辅助可观测：让系统自己发现异常，而不是靠人盯屏幕**
  - 采样与告警：`internal/pkg/metrics` 新增 `window.go`（每 2 秒把累计值换算成速率，保留最近 900 个样本）、`alert.go`（三层判断：绝对下限 `Floor` → 相对基线 `Sigma` 倍标准差 → 连续 `Consecutive` 个窗口确认，恢复时发带持续时长和峰值的恢复事件）、`rules.go`（四条默认规则：突发流量 / 5xx 错误率 / Kafka 积压 / 在途订单积压）、`http.go`（`GET /metrics/history`、`GET /metrics/alerts`）；`metrics.go` 补了完成数、5xx/4xx 计数、平均延迟、当前并发
  - 积压探针：`internal/pkg/kafka/lag.go` 每 5 秒用 `Metadata + ListOffsets + OffsetFetch` 算消费组 LAG 并缓存（**不能用 `Reader.Stats().Lag`，消费组模式下它恒为 -1**），采样器只读缓存
  - 自适应限流：`internal/pkg/ratelimit/adaptive.go` 用 AIMD（健康就每个窗口 +5，吃力就砍 30%，砍完冷却 2 个窗口），阈值夹在 5 ~ 100 且永不超过静态上限；用临时探针验证过「健康→加、延迟 400ms→100/70/49/34/23 连砍、恢复→冷却后 16/21/26/31/36 回升、上限锁死 100、下限锁死 5」
  - 压测报告工具：新增 `cmd/benchreport`（`-k6` / `-url` / `-log` / `-out`），把 k6 视角、服务端视角、系统自报异常、瓶颈线索、原始日志汇成一份 Markdown；瓶颈线索是写死的阈值判断，**刻意不调用大模型**（离线可复现、不依赖外部 API）。样例：`docs/reports/stage63-verify.md`
  - 演示页新增第 ⑤ 区块「系统自己报异常」：每 2 秒轮询 `/metrics/alerts`，显示告警横幅、每条规则的当前值/门槛/平时基线、最近 5 条事件；事件文本用 `textContent` 逐条写，不走 `innerHTML`
  - 实测：256 VU 压 20 秒，系统自己打出「`15:40:05` 突发流量（QPS 4705/s、门槛 500/s）」和「`15:40:27` 恢复（持续 22 秒、峰值 5257.7）」；5xx 峰值 0%、Kafka LAG 峰值 0、在途订单峰值 0；同一条件 A/B（观测前 112,631 请求 / p99 60.0ms，观测后 108,353 请求 / p99 65.18ms），**QPS 无可见下降**
  - 顺手修掉四个真实缺陷（都写进 `docs/interview.md` 了，是很好的面试素材）：① 告警文案和判据不一致（冷启动基线为 0 时报「越过门槛 0/s」）；② 4xx 拒绝率算出 100.43%（分子是「请求结束」、分母是「请求进来」，改了分母口径）；③ 恢复事件的峰值报的是最后一个窗口的瞬时值（586）而不是真实峰值（5257）；④ 报告里事件顺序倒置
- 上一轮文档产出：`docs/benchmark.md` 新增「优化记录 8」（改动表、返回值约定、三个取舍、A/B 对比、CPU profile 结论、一致性复核、已知小风险）；`docs/architecture.md` 更新秒杀时序图（4 次往返 → 1 次 Lua）、三层防线、Redis Key 表（新增 `:window`、限流 key 改按用户）、pprof 开关、第 10 节瓶颈结论；`docs/interview.md` 新增「第 ⑤ 轮优化故事」、数据手册补新数据、重写 Q6（QPS 怎么冲上去的）和 Q8（限流为什么搬进 Lua）、更新不足清单；`docs/build.md` 的 6.1 标记完成并补实测结论
- 下一步计划：阶段 13 已收口。如果继续往下拆，下一步是**阶段 14：把 `internal/user` 拆成独立进程**——新增 `cmd/user`，用 `GATEWAY_UPSTREAM_USER=http://127.0.0.1:8082` 一个环境变量把 `/login` `/register` `/me` 的流量从单体挪到新进程，网关代码和路由表都不用改；同时给下游补上 `trace.Middleware`，让 trace id 真正在日志里串起来。（微服务这部分目前只讨论到「怎么拆」，是否继续拆、拆几块，按需再定）
- 更早两轮的结论（备查）：优化记录 6 用 k6 对照定位出「Gin 自带的逐请求访问日志」是第一个天花板（写文件 6,500/s vs 丢弃 55,000/s，差 9 倍），并用隔离基准量出「一次抢购 4 次 Redis 往返」的上限约 9,000/s；优化记录 7 在不动链路结构的前提下把峰值做到 5,907 QPS，并修掉对账超卖隐患
## 阶段进度

- [x] 阶段 0：环境准备与项目骨架
- [x] 阶段 1：用户模块（注册/登录）
- [x] 阶段 2：商品与库存模块
- [x] 阶段 3：购物车与普通下单
- [x] 阶段 4：秒杀核心模块
- [x] 阶段 5：高并发增强
- [x] 阶段 6：压测与优化（6.1 冲 1 万 QPS、6.2 演示控制台、6.3 AI 辅助可观测全部完成）
- [x] 阶段 7：独立前台与演示内容（`/shop` 商品列表页 + 秒杀独立页 + 我的订单，双屏演示脚本）
- [x] 阶段 8：控制台压测命令生成器（`/demo` 第 ⑥ 块，压测仍由人在终端跑 k6）
- [x] 阶段 9：订单超时关单与库存回补（延时队列 ZSET + 兜底扫描、两层幂等、`idx_user_item` 降级、付款倒计时）
- [x] 阶段 10：滑动窗口限流（ZSET 记放行时刻，抢购热路径与秒杀管理接口两处都改，A/B 实测证明边界放水消失）
- [x] 阶段 11：面试演练与资料收口（k6 脚本进版本库、附录 A/B 补上超时关单与滑动窗口、照着脚本实跑验证）
- [x] 阶段 12：购物车与结算页（/shop/cart 页面 + 商品页加购列；5 个前端函数与购物车越权校验均已实现并实测）
- [x] 阶段 13：网关收口（微服务讨论稿已写完；网关 + trace 骨架已落地，12 条路由 / 6 个服务，6 处空已填完、5 处缺陷已修，六步验证走通）
