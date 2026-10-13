.PHONY: help up up-infra down down-v restart logs logs-server logs-worker ps \
        build build-images migrate test lint run-server run-worker

COMPOSE = docker compose -f docker/docker-compose.yml --env-file .env

help: ## Показать список команд
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

up: ## Поднять весь стек (инфраструктура + server + worker), пересобрать образы
	$(COMPOSE) up -d --build
	@echo "Waiting for services to be healthy..."
	@sleep 5
	$(COMPOSE) ps

up-infra: ## Поднять только инфраструктуру (postgres, minio, rabbitmq)
	$(COMPOSE) up -d postgres minio rabbitmq
	@echo "Waiting for services to be healthy..."
	@sleep 5
	$(COMPOSE) ps

build-images: ## Собрать образы server и worker (без запуска)
	$(COMPOSE) build server worker

migrate: ## Применить миграции к БД (одноразовый контейнер)
	$(COMPOSE) run --rm migrate

down: ## Остановить стек (данные сохраняются)
	$(COMPOSE) down

down-v: ## Остановить + удалить volumes (ПОЛНАЯ ОЧИСТКА)
	$(COMPOSE) down -v

restart: down up ## Перезапустить

logs: ## Логи всех сервисов
	$(COMPOSE) logs -f

logs-server: ## Логи server
	$(COMPOSE) logs -f server

logs-worker: ## Логи worker
	$(COMPOSE) logs -f worker

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