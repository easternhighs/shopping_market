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
