.PHONY: help up down down-v restart logs ps test build lint run-server run-worker

COMPOSE = docker compose -f docker/docker-compose.yml --env-file .env

help: ## Показать список команд
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

up: ## Поднять инфраструктуру (postgres, minio, rabbitmq)
	$(COMPOSE) up -d
	@echo "Waiting for services to be healthy..."
	@sleep 5
	$(COMPOSE) ps

down: ## Остановить инфраструктуру (данные сохраняются)
	$(COMPOSE) down

down-v: ## Остановить + удалить volumes (ПОЛНАЯ ОЧИСТКА)
	$(COMPOSE) down -v

restart: down up ## Перезапустить

logs: ## Логи всех сервисов
	$(COMPOSE) logs -f

ps: ## Статус контейнеров
	$(COMPOSE) ps

test: ## Запустить все тесты
	go test ./... -v

build: ## Собрать все бинари
	go build ./...

lint: ## Запустить golangci-lint
	golangci-lint run

run-server: ## Запустить server локально
	go run ./cmd/server

run-worker: ## Запустить worker локально
	go run ./cmd/worker