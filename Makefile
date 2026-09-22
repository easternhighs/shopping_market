.PHONY: run build run-gateway build-gateway run-user build-user env-up env-down logs kafka-partitions kafka-lag bench-report

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
