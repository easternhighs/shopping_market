# 阶段 6 压测记录

本文件记录压测环境、基线数据、瓶颈和优化前后对比。测试结论会随阶段 6 推进持续更新。

## 测试环境

- 服务：本机 Go 单体服务，`cmd/server`
- 数据库：MySQL 8，`127.0.0.1:3307`
- 缓存：Redis 7，`127.0.0.1:6379`
- 消息队列：Kafka 3.9，`127.0.0.1:9092`
- 压测工具：`cmd/loadtest`，以及临时 Node 脚本用于动态 `request_id` 和多用户 JWT
- 压测注意：秒杀路由配置了单 IP 每秒 100 次的限流；多用户压测时通过不同 `X-Forwarded-For` 模拟不同客户端 IP

## 第一轮基线

| 接口 | 并发 | 时长 | QPS | 成功 | 主要状态码 | P50 | P95 | P99 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `/health` | 100 | 3s | 4588 | 13766 | 200 | 12.25ms | 109.81ms | 193.29ms |
| `/products` | 100 | 3s | 3836 | 11507 | 200 | 8.75ms | 71.21ms | 84.78ms |
| `/seckill/activities` | 100 | 3s | 10166 | 400 | 200:400 / 429:30099 | 9.07ms | 19.34ms | 36.28ms |
| `POST /seckill/items/:id/orders` | 100 | 3s | 817 | 2500 | 200 | 119.40ms | 162.72ms | 196.05ms |

说明：

- `/seckill/activities` 的 429 是限流器正常工作：单 IP 每秒只放行 100 个请求，其余被拒绝。
- 秒杀接口第一轮采用不同用户 token、唯一 `request_id`，全链路通过 Redis 预扣和 Kafka 异步落库，返回 200。
- 压测后对账结果：`seckill_items` 中 `sold=1287`，Redis 剩余库存 `198713`，总库存 `200000`，两边一致。

## 已发现的瓶颈

1. MySQL `max_connections` 默认是 151。
   - 高并发下，GORM 连接池没有设置最大连接数，会尝试建立超过 MySQL 上限的连接，导致 `dial tcp 127.0.0.1:3307: ... refused`。
   - 这是秒杀接口高并发时出现 500 的主要原因之一。

2. 秒杀入口在 Redis 预扣之前先做了两次 MySQL 查询。
   - 一次查 `request_id` 是否重复，一次查“该用户是否已经抢过该商品”。
   - 每秒请求变多后，这两个查询会占满数据库连接，成为吞吐瓶颈。

3. 当前 `cmd/loadtest` 只支持固定 body 和固定 token。
   - 对秒杀接口无法生成唯一 `request_id`，也不方便模拟多用户，因此第一轮多用户压测使用了临时 Node 脚本。

4. `Gin` 默认 debug 模式会打印大量日志。
   - 压测时应使用 `GIN_MODE=release`，否则日志输出本身会拖慢服务。

## 后续优化方向

- 给 `db.Connect` 设置 `SetMaxOpenConns`、`SetMaxIdleConns`、`SetConnMaxLifetime`。
- 在 Docker Compose 中提高 MySQL `max_connections`，例如调整为 500 或 1000。
- 将秒杀入口的防重检查前移到 Redis，MySQL 唯一索引继续作为最终兜底。
- 完善 `cmd/loadtest`，支持动态 `request_id`、自定义 Header 和多用户 token 文件。
- 持续记录 Kafka 消费堆积和数据库连接池水位，作为优化对比指标。

## 优化记录 1：MySQL 连接池与最大连接数

修改内容：

- `internal/pkg/db/db.go` 设置 `MaxOpenConns=100`、`MaxIdleConns=50`、`ConnMaxLifetime=1h`。
- `deployments/docker-compose.yml` 给 MySQL 增加 `--max_connections=500`。

优化前秒杀接口：

| 并发 | QPS | 主要结果 |
| --- | --- | --- |
| 100 | 817 | 全 200，P99 196ms |
| 500 | 1682 | 大量 500，出现 MySQL 连接拒绝 |

优化后秒杀接口：

| 并发 | QPS | P50 | P95 | P99 | 主要结果 |
| --- | --- | --- | --- | --- | --- |
| 100 | 1116 | 86ms | 122ms | 206ms | 全 200 |
| 500 | 1698 | 279ms | 424ms | 573ms | 全 200 |
| 1000 | 1633 | 563ms | 1007ms | 1300ms | 全 200，延迟明显升高 |

结论：

- 连接池和 MySQL 最大连接数优化解决了连接拒绝问题。
- 低并发下吞吐提升，但并发继续增大后，延迟上升、QPS 不再增长。
- 下一个主要瓶颈是秒杀入口在 Redis 之前执行了两次 MySQL 查询，数据库连接池成为吞吐上限。

## 优化记录 2：秒杀入口防重前移到 Redis

修改内容：

- 在 `internal/seckill/cache.go` 新增 `CheckAndPreDeduct`。
- 将 `requestID` 查重、用户限购、库存预扣合并为一次 Redis Lua 原子操作。
- 秒杀接口不再在 Redis 之前查询 `FindOrderByRequestID` 和 `FindOrderByUserAndItem`。

优化前后秒杀接口对比：

| 并发 | 优化前 QPS | 优化前 P99 | 优化后 QPS | 优化后 P99 |
| --- | --- | --- | --- | --- |
| 100 | 1116 | 206ms | 1445 | 153ms |
| 500 | 1698 | 573ms | 2101 | 647ms |
| 1000 | 1633 | 1300ms | 1835 | 1090ms |

本轮压测后的最终一致性：

- 活动 `id=11`、秒杀商品 `id=11`。
- 三轮压测共产生 `29655` 条成功请求。
- Kafka 消费组最终 LAG 归零。
- MySQL `sold=29655`，Redis 剩余库存 `170345`，总库存 `200000`，计算结果一致。

结论：

- 把两次 MySQL 防重查询前移到 Redis 后，低并发吞吐提升约 30%，高并发吞吐也有提升。
- `c=1000` 时延迟仍然较高，说明秒杀入口仍有 `FindItemByID` 和 `FindActivityByID` 两次 MySQL 查询可继续优化。

## 优化记录 3：秒杀商品/活动信息缓存（cache-aside）

修改内容：

- `internal/seckill/cache.go` 实现 `GetItem`/`SetItem`、`GetActivity`/`SetActivity`。
  - 商品信息 key：`seckill:item:{id}:info`。
  - 活动信息 key：`seckill:activity:{id}:info`。
  - TTL 统一 5 分钟（`seckillInfoTTL`）。
  - 修正了原先商品信息错误复用 `seckill:item:{id}:stock`（库存 key）的问题。
- `internal/seckill/service.go` 的秒杀入口改为 cache-aside：
  1. 先读 Redis；
  2. 返回 `redis.Nil`（未命中）时再回源 MySQL，并把结果写回 Redis；
  3. 其他 Redis 错误直接返回，不再拿不确定的数据继续下单。

为什么只缓存“信息”而库存仍走 `:stock`：价格、限购、活动时间改动不频繁，适合缓存几分钟；
库存每秒都在变，必须实时原子扣减，所以继续用专门的 `:stock` key 配 Lua 脚本。

### 验证：热缓存下入口不再查库

验证方法（MySQL 通用日志）：

1. 打开 `general_log`（`log_output=TABLE`）并清空 `mysql.general_log`。
2. 跑一轮 `c=100`、5 秒的秒杀压测，共 `7042` 次请求，全部返回 200。
3. 关闭日志后统计语句。

统计结果：

| 语句 | 次数 | 说明 |
| --- | --- | --- |
| `SELECT ... seckill_items ... WHERE` | 24×3 | 只出现在 03:49:12 / 03:49:42 / 03:50:12，正好是每 30 秒一次的对账任务（12 个商品 × prepare/execute） |
| `SELECT ... seckill_activities ... WHERE` | 0 | 入口完全没有回源 |
| `UPDATE seckill_items ...` | 8332 | 消费端最终扣减库存 |
| `SELECT ... seckill_orders ...` | 16664 | 消费端两条幂等查询，约为消息数 ×2 |

结论：`7042` 次秒杀请求期间，入口对商品/活动一次 MySQL 查询都没有发起，全部命中 Redis 缓存。

### 压测对比

秒杀接口 `POST /seckill/items/12/orders`，总库存 200000，每人限购 1，全量用不同用户 + 唯一 `request_id`：

| 并发 | 优化记录 2 QPS | 优化记录 2 P99 | 本次 QPS | 本次 P99 |
| --- | --- | --- | --- | --- |
| 100 | 1445 | 153ms | 1848 | 110ms |
| 500 | 2101 | 647ms | 2175 | 1062ms |
| 1000 | 1835 | 1090ms | 2938 | 732ms |

结论：

- `c=100` 吞吐提升约 28%，P99 从 153ms 降到 110ms。
- `c=1000` 吞吐提升约 60%（1835 → 2938），P99 从 1090ms 降到 732ms。
- `c=500` 吞吐基本持平但 P99 升高，属于单次采样的抖动，需要后续多次复测确认。

### 最终一致性校验

- 活动 `id=12`、秒杀商品 `id=12`，总库存 `200000`。
- 5 轮压测共 `60431` 次成功请求。
- Kafka 消费组 LAG 最终归零。
- MySQL `sold=60431`，`seckill_orders` 中该商品订单数 `60431`。
- Redis 剩余库存 `139569`，`200000 - 60431 = 139569`，两边完全一致。

## 优化记录 4：消费端吞吐（骨架已就绪，待填写）

秒杀入口优化后，瓶颈转移到了消费端。观察到的现象：

- Kafka 只有 1 个分区，消费端只有 1 个 goroutine，一条消息一个 MySQL 事务。
- `kafka-go` 默认 `CommitInterval=0`，相当于每消费一条就同步提交一次 offset。
- 实测消费速率只有约 50～360 条/秒；`60431` 条消息需要约 20 分钟才追平。
- Windows 上 Docker Desktop 的端口转发放大了 Kafka/MySQL 的往返延迟，进一步压低消费速率。

### 这一轮改了什么（骨架）

| 位置 | 改动 | 目的 |
| --- | --- | --- |
| `deployments/docker-compose.yml` | 新增 `KAFKA_NUM_PARTITIONS: "6"` | 新建的 topic 默认 6 个分区 |
| Kafka 的 `seckill_orders` | 分区数 1 → 6（已执行 `--alter`） | 并行消费的上限从 1 提到 6 |
| `Makefile` | 新增 `kafka-partitions` / `kafka-lag` | 一条命令改分区、看堆积 |
| `internal/pkg/kafka/kafka.go` | 生产者加 `BatchSize=100`、`BatchTimeout=10ms`；消费者加 `MinBytes`/`MaxBytes`/`MaxWait`/`QueueCapacity`，新增 `FetchBatch` + `Commit`，去掉原来的单条 `Read` | 攒批发送、批量拉取、批量提交 |
| `internal/seckill/queue.go` | `Consume` → `ConsumeBatch`，新增 `ConsumedOrder`（同时带原始消息和解析错误）；`Queue` 支持持有多个消费者 | 一批消息能带着“待提交的位置”走完整条流程 |
| `internal/seckill/worker.go` | `ConsumeOrders` 改成给每个消费者开一个 goroutine；新增 `consumeLoop` 空壳 | 多个消费者同时干活 |
| `cmd/server/main.go` | 创建 6 个属于同一消费组的消费者 | 把并发消费接起来 |

### 本轮作业：由你来填的部分

每个函数上方都写了详细的实现思路，照着写即可：

1. `internal/pkg/kafka/kafka.go` 的 `FetchBatch`：循环 `FetchMessage` 凑一批，拉不满就超时收工。
2. `internal/pkg/kafka/kafka.go` 的 `Commit`：把一批消息的消费进度一次性提交。
3. `internal/seckill/queue.go` 的 `ConsumeBatch` / `Commit`：解析 JSON，并把原始消息一条不差地带上。
4. `internal/seckill/worker.go` 的 `consumeLoop`：批量拉取 → 逐条落库 → 批量提交。

### 两个必须讲清楚的取舍

- **`CommitInterval` 保持 0**：它等于 0 时 `CommitMessages` 才是“真提交”（会等 Kafka 确认）。
  我们改成一批提交一次，网络往返从 N 次降到 1 次，同时保住“落库成功才提交”的可靠性。
  如果把它设成非 0，kafka-go 会改成后台异步提交：更快，但进程崩溃时可能丢掉还没提交的一批消息。
- **批量提交之后，失败的消息不会自动重投**：所以处理失败时必须先 `compensate`，
  把 Redis 预扣的库存和购买数还回去；否则会出现“Redis 少了库存、MySQL 又没有订单”。
  最后的正确性由 30 秒一次的对账任务兜底。

### 怎么验证这轮优化

1. 填完 TODO 后先过编译和检查：`go build ./... && go vet ./...`。
2. 启动服务，压测前后都看一眼消费组：`make kafka-lag`。
3. 跑一轮压测（例如 c=500、10 秒），重点看三个数：
   - 消费速率 = 总消息数 ÷ 追平耗时（优化前只有 50～360 条/秒）；
   - LAG 归零所需时间；
   - 服务进程 CPU 是否真的用上了多核（`docker stats` 看不到，用任务管理器看 `go run` 进程）。
4. 压测结束再核对一致性：MySQL 的 `sold`、订单数、Redis 剩余库存三者要对得上。

### 还没做的（留给下一轮）

- 合并事务：把“两条幂等查询 + 扣库存 + 插订单”合并成批量语句，一个事务处理一批消息。
- 换成真正的批量写入（批量 `INSERT ... VALUES`、`ON DUPLICATE KEY UPDATE`）。

## 优化记录 5：批量落库（已完成）

第 4 轮改的是 Kafka 侧（6 分区、6 消费者、批量拉取、批量提交），MySQL 侧没动：
**仍然是一条消息一个事务**。于是瓶颈从 Kafka 转移到了 MySQL 的行锁上。

### 实测现象（优化前）

- 消费速率只有约 **80 条/秒**（压测 15 秒：LAG 只降了 1200，`sold` 增加 1139）。
- 15 秒内 `Com_commit` 增加 1057，而 `Innodb_row_lock_waits` 也增加 1057：每次提交都在等行锁。
- `Innodb_row_lock_time_avg = 65`（平均每次等锁 65 毫秒），`Innodb_row_lock_current_waits` 有 5 个在排队。
- 原因：每个事务里都有一句 `UPDATE seckill_items SET sold = sold + 1 WHERE id = ?`，
  6 个消费者改的是**同一行**；MySQL 同一行同一时刻只允许一个事务修改，
  于是 6 条流水线全挤在一个收银台前排队。

### 这一轮改了什么

把“一批消息 = N 个事务”改成“一批消息 = 1 个事务”：

| 原来（每条消息一次） | 现在（整批一次） |
| --- | --- |
| N 次 `SELECT ... WHERE request_id = ?` | 1 次 `WHERE request_id IN (...)` |
| N 次 `SELECT ... WHERE user_id = ? AND item_id = ?` | 每个 itemID 1 次 `IN (...)` |
| N 次 `UPDATE ... SET sold = sold + 1` | 每个 itemID 1 次 `SET sold = sold + N` |
| N 次单条 `INSERT` | 1 次多值 `INSERT ... VALUES (...),(...)` |
| N 次行锁竞争 | 1 次行锁竞争 |

| 位置 | 改动 |
| --- | --- |
| `internal/seckill/repository.go` | 新增 4 个批量方法：`FindOrdersByRequestIDsWithTx`、`FindOrdersByUserAndItemWithTx`、`CreateOrdersWithTx`、`DeductStockBatch` |
| `internal/seckill/service.go` | 新增 `BatchResult` 结果清单和 `processOrdersBatch`（整批一个事务） |
| `internal/seckill/worker.go` | `consumeLoop` 从“逐条落库”改成“整批落库”，再按结果清单分类上报/回补 |

### 实现要点

- `DeductStockBatch`：一条 `UPDATE ... SET sold = sold + ? WHERE id = ? AND sold + ? <= total_stock`
  扣掉整批数量，`RowsAffected == 0` 才返回 `ErrInsufficientStock`；
  先判断 `result.Error`，避免 SQL 出错被误判成“库存不足”。
- `processOrdersBatch`：整批一个事务，顺序是
  ① 一条 `IN` 查重 → ② 按 itemID 分组批量查限购 → ③ 每个 itemID 调一次 `DeductStockBatch`
  → ④ 一次多值 `INSERT` → ⑤ 填 `BatchResult`。
- 额外做了“批内去重”：同一批里出现相同 `request_id`，或同一用户对同一商品出现两条消息，
  提前判成重复（否则会撞唯一索引把整批插入打回）。

### 实测结果（优化后）

| 压测 | 请求数 | QPS | P99 | LAG |
| --- | --- | --- | --- | --- |
| c=100 / 8s | 15379 | 1915 | 126ms | 归零 |
| c=200 / 10s | 20228 | 2012 | 233ms | 全程 0 |
| c=600 / 15s | 29236 | 1921 | 620ms | 峰值 368，约 3 秒追平 |

三次压测都是 `200` 状态码、0 错误。行锁与提交次数对比：

| 指标 | 优化前 | 优化后（15379 条消息） |
| --- | --- | --- |
| `Com_commit` | 1057 次 / 1057 条消息 | 196 次 / 15379 条消息（约 78 条一个事务） |
| `Innodb_row_lock_waits` | 1057 | 90 |
| 消费速率 | 约 80 条/秒 | 实测已能持续跟上约 1900 条/秒的写入 |

一致性核对（第一轮压测后）：`sold` 23333 → 38712，订单数 38712（无重复用户），
Redis 剩余 176667 → 161288，而 `200000 - 38712 = 161288`，两边完全对得上。

注意：这几轮 QPS 停在 1900 左右是**压测客户端和请求入口路径的上限**，
不是消费端的上限——消费端始终没有积压。要冲 1 万 QPS，下一步要压的是
HTTP 入口（Redis Lua + Kafka 发送这一段）和压测工具本身。

### 两个容易踩的坑

- **重复消息不要回补 Redis**：所谓“重复”是 MySQL 里已经有这条订单，
  说明当初那次扣减是真实成交的；再回补一次，Redis 库存就会凭空多出来，等于超卖。
- **整批失败就整批回补**：扣库存和插订单在同一个事务里，任何一步出错都会整体回滚，
  这一批消息统一记 failure 并回补 Redis。因为 Redis 在入口已经预扣过，
  理论上极少发生；真出现了由 30 秒一次的对账任务兜底。

### 怎么验证这轮优化

1. 先过编译和检查：`go build ./... && go vet ./...`。
2. 启动服务跑一轮压测，对比消费速率（优化前 80 条/秒）。
3. 看行锁指标：`Innodb_row_lock_waits` 应该远小于 `Com_commit`。
4. 压测结束核对三者一致：MySQL 的 `sold`、该商品的订单数、Redis 剩余库存。

## 阶段 6 收尾核对（多轮压测累积后）

把四轮压测（含批量落库前后的全部轮次）的数据累积在同一个商品上，做一次总核对。
商品：秒杀商品 `id=14`，总库存 `200000`，每人限购 1 件。

| 项目 | 数值 |
| --- | --- |
| MySQL `seckill_items.sold` | 105092 |
| MySQL `seckill_orders` 订单数 | 105092 |
| MySQL `seckill_orders` 去重用户数 | 105092 |
| Redis `seckill:item:14:stock` 剩余库存 | 94908 |
| 计算结果 `200000 - 105092` | 94908 |

结论：

- 四个数字完全自洽：卖出的件数 = 订单数 = 去重用户数，剩余库存 = 总库存 - 卖出件数。
- **没有超卖**：`DeductStockBatch` 的 `sold + N <= total_stock` 条件更新守住了最后一道闸门。
- **没有重复购买**：订单数等于去重用户数，说明每人限购 1 件在 Redis 入口和 MySQL 唯一索引两层都生效。
- **Redis 与 MySQL 最终一致**：两边算出来的剩余库存一模一样。

## 附：唯一索引核对（关闭一处遗留疑问）

之前用 `SHOW INDEX` 看 `seckill_orders` 时，曾怀疑 `request_id` 和 `(item_id, user_id)`
两个唯一索引实际是普通索引（即模型里的 `uniqueIndex` 没落到数据库上）。
用 `information_schema.STATISTICS` 复核后确认：这是**误读**。

`SHOW INDEX` 的第二列是 `Non_unique`，`0` 表示**是**唯一索引，`1` 才表示普通索引，
当时把 `0` 当成了“非唯一”。实际结果：

| 表 | 索引 | 列 | 类型 |
| --- | --- | --- | --- |
| `seckill_orders` | `idx_seckill_orders_request_id` | `request_id` | UNIQUE |
| `seckill_orders` | `idx_user_item` | `item_id, user_id` | UNIQUE |
| `seckill_items` | `idx_activity_sku` | `activity_id, sku_id` | UNIQUE |
| `orders` | `idx_orders_request_id` | `request_id` | UNIQUE |

也就是说，模型里声明的唯一索引在数据库里全部到位，
“MySQL 唯一索引作为防超卖/防重复的最后兜底”这条设计是成立的，不需要修表。

## 优化记录 6：入口访问日志的吞吐代价（k6 定位出的第一瓶颈）

### 起因：每轮压测都卡在约 2000/s

前面几轮无论怎么加并发，秒杀接口的吞吐都停在 2000/s 上下不动，
p50 稳定在 14ms 左右。**吞吐不动、延迟也几乎不涨**——这不是"服务被打满"的形状，
更像是某处存在一个固定速率的串行环节。本轮换成 k6 重新排查，
并第一次把"服务的日志写到哪里"当作被测量的变量而不是背景噪音。

### 对照实验：访问日志写文件 vs 直接丢弃

`cmd/server/main.go` 里用的是 `gin.Default()`。它等于 `gin.New()` 再加两个中间件：

- `Logger`：把每一行访问日志写出去（默认目标 `os.Stdout`）；
- `Recovery`：panic 兜底。

也就是说，**每一个 HTTP 请求都会触发一次写日志动作**，秒杀接口也不例外。
同一份业务代码、同一个压测脚本，只改变 stdout 的去向：

| 压测项（`/health`，12 秒） | stdout = 桌面目录下的日志文件 | stdout 重定向到 `NUL` |
| --- | --- | --- |
| 64 VU | 64,600 次（约 5,400/s） | — |
| 128 VU | 76,000 次（约 6,300/s） | — |
| 256 VU | 78,000 次（约 6,500/s） | **661,414 次（约 55,000/s）** |
| 384 VU | — | 632,543 次（约 52,700/s），p50 4.8ms |

**同一份业务代码，只把访问日志从"写文件"改成"丢掉"，吞吐相差约 9 倍。**

> 排查过程中踩的坑：这台机器上 `Get-NetTCPConnection -LocalPort 8080`
> 经常返回空结果，导致"先停掉旧服务"的命令静默失败，旧进程一直占着 8080 端口，
> 于是第一次 A/B 对比量到的其实还是旧进程，结论无效。
> 后来改成用 `Get-Process -Name server` 查看 PID 和启动时间，
> 确认进程确实换成了新的，才重新测出真实差距。

这条结论对项目本身的意义：**访问日志不是"免费"的**，它是入口路径上的同步 I/O。
生产环境里通常靠"日志写本地高速设备 + 异步刷盘 + 采样"来摊薄，而不是每条都同步写。

### 去掉日志后：秒杀接口的饱和曲线

把服务以"stdout 重定向到 `NUL`"的方式重启（新进程）后，用 k6 跑秒杀接口，
`ramping-vus` 先 2 秒爬到目标并发、再保持 10 秒，每轮用全新的用户号和请求号（命名空间隔离）：

| 并发 VU | 12 秒总请求数 | 稳态吞吐（约 /11s 折算） | p50 | p95 | p99 | 错误分类 |
| --- | --- | --- | --- | --- | --- | --- |
| 64 | 35,298 | 约 3,200/s | 19.8ms | 23.0ms | 24.4ms | 全 200 |
| 128 | 42,909 | 约 3,900/s | 32.1ms | 47.3ms | 56.3ms | 全 200 |
| 256 | 50,446 | 约 4,600/s | 54.6ms | 74.9ms | 100.8ms | 全 200 |
| 512 | 55,749 | **约 5,100/s** | 100.3ms | 133.3ms | 146.5ms | 全 200 |
| 768 | 54,002 | 约 4,900/s | 158.2ms | 196.3ms | 222.9ms | 全 200 |
| 1024 | 50,379 | 约 4,600/s | 222.1ms | 299.0ms | 369.9ms | 全 200 |

读法：

- 512 VU 是拐点。再往上加并发，**吞吐不再涨（甚至略降），只有延迟继续线性上涨**——
  这是典型的"已经饱和、多余的并发只是在排队"。
- 稳态吞吐和利特尔法则对得上：`512 ÷ 0.1003s ≈ 5,105/s`，与表中 5,100/s 一致。
- 这一轮**没有任何业务错误**：`rateLimited / perUser / stock / dupReq / otherBiz / connErr` 全部为 0。
- 服务端 CPU 采样：512 VU 期间消耗 72.05s CPU / 20.24s 墙钟 ≈ 3.56 个核（机器 20 逻辑核），
  **CPU 也没有打满**。所以瓶颈不是"CPU 不够"，而是"请求在等某个东西"。

### 继续定位：剩下的瓶颈是"每次请求的 Redis 往返次数"

秒杀一次请求的 Redis 操作依次是：

1. 限流：`INCR` 计数（首次还要 `EXPIRE`）；
2. 读商品缓存：`GET seckill:item:16:info`；
3. 读活动缓存：`GET seckill:activity:16:info`；
4. 预扣库存：`EVAL` 执行 Lua（校验时间窗之外的全部库存逻辑）。

这是 **4 次顺序往返**（每跳都要等 Redis 回包才能发下一条）。
为了确认它是不是天花板，写了一个隔离基准 `.tmp/rbench`，
复刻同样的 4 跳（`hops` 参数控制顺序跳数，`-pipe` 则把 4 条命令合并成一次往返发送）：

| 场景（conc=256，纯 Redis） | 吞吐 | p50 |
| --- | --- | --- |
| 4 跳顺序（= 当前代码） | 9,736/s | 25.6ms |
| 3 跳顺序（去掉限流那一跳） | 12,596/s | 19.6ms |
| 1 跳顺序（只留 Lua） | 28,964/s | 8.1ms |
| **4 条命令合并成 1 次往返** | **27,822/s** | **8.5ms** |

**4 条命令合并发送（一次往返）与"只发 1 条命令"的吞吐几乎相同（27,822 vs 28,964），
是当前 9,736/s 的约 2.9 倍。** 这说明 4 跳里绝大部分时间花在"等回包"，
而不是 Redis 本身的处理能力（容器内 `redis-benchmark` 有 13~15 万 rps，主机到容器单次 PING 仅 0.45ms）。

把 Kafka 写也叠上去（完整链路：限流 + 2 次缓存读 + Lua + 发消息）：

| 场景（完整链路） | conc=256 | conc=512 |
| --- | --- | --- |
| 4 跳顺序 + 批次超时 10ms（= 当前代码） | 8,755/s，p50 29.5ms | 9,389/s，p50 53.8ms |
| 3 跳顺序 + 10ms | 11,812/s，p50 21.1ms | 12,271/s，p50 41.1ms |
| 合并成 1 次往返 + 10ms | 20,211/s，p50 12.1ms | **27,634/s，p50 17.3ms** |
| 合并成 1 次往返 + 批次超时 1ms | 25,732/s，p50 9.2ms | 25,971/s，p50 18.6ms |

结论：**"把 4 次 Redis 往返压成 1 次"这一项，就能把入口链路的隔离上限从约 9,000/s 拉到约 27,000/s（约 3 倍）。**
这是通往 1 万 QPS 目标的必经一步；Kafka 的 `BatchTimeout` 从 10ms 降到 1ms 主要收益是**降低延迟**（p50 约降 3ms），对吞吐影响不大。

### 数据一致性复核（本轮压测后）

商品 `id=16`，总库存 5,000,000，每人限购 1 件：

| 项目 | 数值 |
| --- | --- |
| MySQL `seckill_items.sold` | 823,971 |
| MySQL `seckill_orders` 订单数 | 823,971 |
| MySQL `seckill_orders` 去重用户数 | 823,971 |
| Redis `seckill:item:16:stock` 剩余库存 | 4,176,029 |
| 计算结果 `5000000 - 823971` | 4,176,029 |

- 四个数字完全自洽：订单数 = 去重用户数 = 已售件数，剩余库存 = 总库存 − 已售件数。
- Kafka 消费组 `shopping_market_seckill` 的 6 个分区 **LAG 全部为 0**，消息已全部落库。
- 注意：高压下**没有超卖、没有重复购买**——这条在面试里比吞吐数字更有说服力。

### 下一步（待确认后实施）

> 落地情况见下一节「优化记录 7」：第 1、3、4、5 项，以及那处 HTTP 语义缺陷，都已经做完；
> 第 2 项（合并 Redis 往返）和第 6 项（库存分桶）还没做。

按"收益 / 风险"排序：

| # | 改动 | 预期收益 | 风险 |
| --- | --- | --- | --- |
| 1 | `gin.Default()` → `gin.New()` + `gin.Recovery()`（或不写访问日志） | 已实测约 9 倍 | 低：只丢访问日志，业务不变 |
| 2 | 限流 + 两次缓存读 + Lua 合并成一次 Redis 往返 | 隔离上限约 9,000/s → 27,000/s | 中：需改动 Lua 与调用顺序，要重跑全量回归 |
| 3 | Kafka `BatchTimeout` 10ms → 1ms | p50 约降 3ms | 低：批次变小，吞吐略降但可接受 |
| 4 | `http.Server` 补显式超时（现在 `r.Run()` 无任何超时） | 抗慢连接 | 低 |
| 5 | Redis 客户端连接池 `PoolSize` | 高并发下更稳 | 低 |
| 6 | 库存分桶 | 上限进一步抬高 | **高：会破坏"总数精确"这一正确性前提** |

另外发现一处**语义缺陷（不属于性能，但影响接口正确性）**：
`internal/seckill/handler.go` 的 `Seckill` 把 service 层返回的所有错误统一映射成 HTTP 500。
"超出每人购买限制""重复的 request_id""秒杀库存不足""已抢购"这些其实都是**业务拒绝**，
按 HTTP 语义应该是 4xx（如 409 / 429）。当前压测脚本只能靠错误文案来分类，正是因为这个。
## 优化记录 7：落地 6 项低风险修复 + 修掉一处对账超卖隐患（已完成）

优化记录 6 结尾列出的候选项里，收益高、风险低的 1/3/4/5 项和那处 HTTP 语义缺陷本轮全部落地；
第 2 项（合并 Redis 往返）和第 6 项（库存分桶）本轮没做，理由见本节最后。

### 这一轮改了什么

| 文件 | 改动 | 解决什么问题 |
| --- | --- | --- |
| `cmd/server/main.go` | `gin.Default()` 换成 `gin.New() + gin.Recovery()`，自写 `accessLog()` 中间件：只有状态码 >= 400 或耗时 >= 500ms 的请求才写一条日志 | 访问日志是上一轮的吞吐天花板；改成「只记异常」后既拿回吞吐，又不至于真出事时没日志 |
| `cmd/server/main.go` | `r.Run()` 换成显式 `http.Server`，补 `ReadHeaderTimeout / ReadTimeout / WriteTimeout / IdleTimeout`，并加信号优雅退出 | 原来没有任何超时，慢连接可以一直占着连接和 goroutine；退出时会直接掐断处理到一半的请求 |
| `cmd/server/main.go` | `gin.SetMode(gin.ReleaseMode)` | Debug 模式有额外开销和噪音 |
| `internal/seckill/service.go`、`cache.go`、`handler.go` | 业务拒绝改成错误哨兵（`ErrInsufficientStock`、`ErrPerUserLimitExceeded`、`ErrDuplicateRequest`、`ErrActivityNotStarted / Ended`、`ErrItemNotFound`、`ErrInvalidRequest`），handler 用 `seckillErrorStatus()` 映射状态码 | 修掉「所有错误都返回 500」的语义缺陷：参数错 400、找不到 404、业务拒绝 409、兜底 500 |
| `internal/pkg/kafka/kafka.go` | 生产者 `BatchTimeout` 10ms 改成 1ms | 这个值会变成每个请求的固定等待；改小后延迟明显下降 |
| `internal/pkg/redis/redis.go` | 连接池 `PoolSize=512`、`MinIdleConns=16`、`PoolTimeout=2s` | 高并发下不再排队等连接 |
| `internal/seckill/queue.go`、`reconcile.go` | 给队列加「在途订单计数 + 消费端是否追平」两个信号，对账只在两者都满足时才覆盖 Redis；顺手修掉 `ListItems` 出错就 `return`、导致对账线程永久退出的问题 | 见下一小节，这是本轮唯一一处**正确性**修复 |

### 关键：对账任务在「有订单在途」时会凭空加回库存（已修）

这是上一轮遗留的疑问，本轮用压测数据复现，确认它是**真实缺陷**，不是理论风险。

原因：对账是拿 MySQL 的 `sold` 反推 Redis 库存（剩余 = 总库存 - 已售）。
只要还有订单在途中（Redis 已经预扣、MySQL 还没记账），`sold` 就是偏小的，
反推出来的剩余库存就是偏大的；把这个偏大的数字写回 Redis，
等于把已经卖掉的库存又变出来一次。

实测证据（修复前）：压测结束、Kafka 里还剩 10,634 条没消费完时，刚好撞上一次对账，
Redis 剩余库存被写成 `4,625,561`，而按 MySQL 账本应该是 `4,614,927`，
**凭空多出 10,634 个名额**。同样的比例放到「库存 100 件」的秒杀里，就是实打实的超卖。

修复：`Queue` 维护两个信号。

- `pending`：已经发进 Kafka、但消费端还没提交落库的订单条数（发布 +1，提交 -len(batch)）。
- `idle`：每个消费者最近一次拉取是不是空的（拉空说明它负责的分区已经追平）。

`Queue.Settled()` 要求「`pending == 0` 且所有消费者都追平过」才返回 true；
`ReconcileItem` 在 `Settled()` 为 false 时直接跳过，`ReconcileLoop` 打一条日志说明本轮跳过。

为什么要看 `idle`：进程刚重启时 `pending` 是 0（这个进程还没发过消息），
但 Kafka 里可能还压着上一轮没消费完的消息，只看 `pending` 会误判成「队列安静」。

### 验证：库存不会再被加回去

修复后重跑同样的爆发流量（512 VU x 60s，期间对账触发 3 次），
采样 `delta = Redis剩余 - (总库存 - MySQL已售)`：

| 采样时刻 | delta | 说明 |
| --- | --- | --- |
| 压测中（每 5 秒一次，共 13 次） | 从 -13,907 一路到 -35,283 | 全程为负：Redis 只会比 MySQL 账本**更少**，不会更多 |
| 压测停止后 40 秒 | 0 | 消费端追平后对账补齐，两边精确一致 |

日志里能看到 3 次 `秒杀对账跳过本轮：仍有订单在途中，等待消费端追平`（23:53:24 / 23:53:54 / 23:54:24），
正好对应压测期间的 3 个对账周期。**没有这次修复的话，这 3 次每次都会往 Redis 里加回库存。**

### 优化前后的吞吐对比

同一台机器、同一个脚本、同样在 512 VU 拐点附近（HOLD=10s）：

| 并发 | 修复前 QPS | 修复后 QPS | 变化 |
| --- | --- | --- | --- |
| 64 | 3,200 | 4,253 | +33% |
| 128 | 3,900 | 4,712 | +21% |
| 256 | 4,600 | 5,623 | +22% |
| 512 | 5,100 | **5,907** | +16% |
| 768 | 4,900 | 5,762 | +18% |
| 1024 | 4,600 | 5,831 | +27% |

- 拐点仍在 512 VU 附近，但曲线更平了：768 / 1024 VU 不再掉吞吐，只涨延迟。
- 512 VU 时 p50 78.6ms、p95 97.0ms、p99 108.3ms，全部 200、0 错误、0 限流。
- 对照组：`/health` 在同一台机器上峰值约 **64,000/s**（64 VU 时 12 秒完成 767,768 次请求）。
  说明 **HTTP 入口离瓶颈还差 10 倍，5,900/s 完全是秒杀业务链路自己造成的**，
  和上一轮「4 次顺序 Redis 往返，隔离上限约 9,000/s」的结论对得上。

### 数据一致性复核（本轮压测后）

商品 `id=17`，总库存 5,000,000，每人限购 1 件：

| 项目 | 数值 |
| --- | --- |
| MySQL `seckill_items.sold` | 860,774 |
| MySQL `seckill_orders` 订单数 | 860,774 |
| MySQL `seckill_orders` 去重用户数 | 860,774 |
| MySQL `seckill_orders` 去重 request_id 数 | 860,774 |
| Redis `seckill:item:17:stock` 剩余库存 | 4,139,226 |
| 计算结果 `5000000 - 860774` | 4,139,226 |

- 四个数字完全自洽，Redis 也对得上：**没有超卖、没有重复购买**。
- Kafka 消费组 `shopping_market_seckill` 的 6 个分区 **LAG 全部为 0**。
- 全程日志里没有 panic、没有「补偿失败」、没有消费端退出。

### 本轮没做的两项

| # | 改动 | 为什么没做 |
| --- | --- | --- |
| 2 | 限流 + 两次缓存读 + Lua 合并成一次 Redis 往返 | 要重排入口的调用顺序、改 Lua 的 KEYS/ARGV 约定，属中等风险改动，需要单独一轮加全量回归 |
| 6 | 库存分桶 | 会破坏「总数精确」这个正确性前提，收益不确定、风险最高 |

也就是说：**在不动入口链路结构的前提下，吞吐已经提到约 5,900/s；要冲 1 万，下一步必须做第 2 项。**
