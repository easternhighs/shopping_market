.PHONY: run build env-up env-down logs

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
