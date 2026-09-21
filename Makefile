.PHONY: run build run-gateway build-gateway env-up env-down logs kafka-partitions kafka-lag bench-report

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
