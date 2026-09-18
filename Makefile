.PHONY: run build env-up env-down logs kafka-partitions kafka-lag

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

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
