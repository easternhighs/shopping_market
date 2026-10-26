.PHONY: run build run-gateway build-gateway run-user build-user run-seckill build-seckill run-product build-product run-order build-order run-shop build-shop run-console build-console env-up env-down logs kafka-partitions kafka-lag bench-report

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

# 阶段 13：API 网关。它站在 cmd/server 前面当统一入口，长跑在另一个端口上（默认 :8081）。
# 网关是旁路式的：不启动它，cmd/server 也照常工作，压测脚本继续直连 8080。
run-gateway:
	go run ./cmd/gateway

build-gateway:
	go build -o bin/gateway ./cmd/gateway

# 阶段 14：用户服务。把 internal/user 搬成独立进程（默认 :8082）。
# 要让它真正接管流量，需要两件事配合：
#   1) 网关那边设 GATEWAY_UPSTREAM_USER=http://127.0.0.1:8082，
#      网关才会把 /login /register /me 转过来（网关代码一行都不用改）；
#   2) 本服务设 TRUST_USER_ID_HEADER=1，它才会直接采用网关注入的 X-User-Id。
#      不设也能正常工作（自己再验一遍 JWT），只是白多一次验签。
run-user:
	go run ./cmd/user

build-user:
	go build -o bin/user ./cmd/user

# 阶段 15：秒杀服务。把 internal/seckill 搬成独立进程（默认 :8083）。
# 要让它真正接管流量，需要三件事配合：
#   1) 网关那边设 GATEWAY_UPSTREAM_SECKILL=http://127.0.0.1:8083，
#      网关才会把 /seckill、/metrics 转过来（网关代码一行都不用改）；
#   2) 单体那边设 SECKILL_MODULE=0，让它别再提供秒杀接口、也别再跑秒杀的后台任务
#      （两个进程同时跑对账会算错库存，原因见 docs/build.md 阶段 15）；
#   3) 本服务设 SECKILL_TRUST_USER_ID_HEADER=1，它才会直接采用网关注入的 X-User-Id。
#      直接压 8083 做基线时【不要】设它 —— 那时要自己验 JWT，成绩才和阶段 6 可比。
run-seckill:
	go run ./cmd/seckill

build-seckill:
	go build -o bin/seckill ./cmd/seckill

# 阶段 16：商品服务。把 internal/product + internal/inventory 搬成独立进程（默认 :8084）。
# 要让它真正接管流量，需要三件事配合：
#   1) 网关那边设 GATEWAY_UPSTREAM_PRODUCT=http://127.0.0.1:8084，
#      网关才会把 /products、/stocks 转过来（网关代码一行都不用改）；
#   2) 单体那边设 PRODUCT_MODULE=0，让它别再提供这两个模块的路由，
#      同时订单服务的扣库存请求会自动改打 :8084（见 cmd/server/main.go 的
#      orderDefaultDeductURL），不需要额外配置；
#   3) 本服务不需要改鉴权配置 —— 它的所有接口本来就不需要登录。
# 注意：/internal/stocks/deduct 是给订单服务用的内部接口，网关不认识 /internal 前缀，
# 所以外网访问它只会 404；调试时可以直接打 http://127.0.0.1:8084/internal/stocks/deduct。
run-product:
	go run ./cmd/product

build-product:
	go build -o bin/product ./cmd/product

# 阶段 17：订单服务。把 internal/order + internal/cart + internal/payment 三块
# 搬成独立进程（默认 :8085）。要让它真正接管流量，需要三件事配合：
#   1) 网关那边设 GATEWAY_UPSTREAM_ORDER=http://127.0.0.1:8085，
#      网关才会把 /orders、/cart、/payments 转过来（网关代码一行都不用改）；
#   2) 单体那边设 ORDER_MODULE=0，让它别再提供这三组路由、
#      也别再跑那个库存补偿任务（两个进程同时扫同一张 outbox 表不会重复扣，
#      但没必要让两个进程做同一件事）；
#   3) 本服务设 TRUST_USER_ID_HEADER=1，它才会直接采用网关注入的 X-User-Id。
# 另外这个服务要能扣库存，前提是商品服务（:8084）活着 —— 打哪个地址由
# PRODUCT_SERVICE_URL 决定，不设就默认 http://127.0.0.1:8084。
run-order:
	go run ./cmd/order

build-order:
	go build -o bin/order ./cmd/order

# 阶段 18：独立前台。只发页面、拼页面数据，不连 MySQL / Redis / Kafka。
# 要让它接管流量：单体设 SHOP_MODULE=0，网关设
# GATEWAY_UPSTREAM_SHOP=http://127.0.0.1:8086。
# 它调商品 / 秒杀服务，地址分别由 PRODUCT_SERVICE_URL、SECKILL_SERVICE_URL 决定。
run-shop:
	go run ./cmd/shop

build-shop:
	go build -o bin/shop ./cmd/shop

# 阶段 18：独立演示控制台。它的一键造数会跨服务调用用户 / 商品 / 秒杀。
# 要让它接管流量：单体设 CONSOLE_MODULE=0，网关设
# GATEWAY_UPSTREAM_CONSOLE=http://127.0.0.1:8087。
run-console:
	go run ./cmd/console

build-console:
	go build -o bin/console ./cmd/console

env-up:
	docker compose -f deployments/docker-compose.yml up -d

env-down:
	docker compose -f deployments/docker-compose.yml down

logs:
	docker compose -f deployments/docker-compose.yml logs -f

# 给已经建好的 seckill_orders topic 增加分区。
# 分区是并发消费的上限：1 个分区只能有 1 个消费者在干活。
# 注意分区数只能增加不能减少；增加后消息在分区之间的先后顺序不再保证，
# 但本项目的每条消息都带自己的 request_id、互相独立，所以不受影响。
kafka-partitions:
	docker exec shopping_market_kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --alter --topic seckill_orders --partitions 6

# 查看消费组堆积情况：LAG 归零说明消费端已经追上生产速度。
kafka-lag:
	docker exec shopping_market_kafka /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --group shopping_market_seckill --describe

# 汇总一份压测报告：k6 的 summary、服务端指标（/metrics*）、服务日志三份材料拼成一份 Markdown。
# 用法：make bench-report K6=.tmp/k6_summary.json LOG=.tmp/srv_demo.log
# 不传参数也能跑（服务还活着的话，至少能出服务端那一节）；报告打印在屏幕上，方便直接贴。
bench-report:
	go run ./cmd/benchreport -k6 "$(K6)" -log "$(LOG)" -out -
