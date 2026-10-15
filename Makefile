.DEFAULT_GOAL := help

SHELL := /bin/sh

COMPOSE := docker compose -f docker/docker-compose.yml --env-file .env
GO := go

.PHONY: help check-env check-docker up up-no-wait up-infra down down-v restart \
        rebuild-app logs logs-server logs-worker logs-infra ps config \
        build build-binaries build-images \
        test test-v test-cover test-race lint vet fmt fmt-check clean clean-all \
        run-server run-worker deps

# ─── Help ────────────────────────────────────────────────────────────────────

help: ## Показать список команд
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ─── Checks ──────────────────────────────────────────────────────────────────

check-env: ## Проверить наличие .env
	@if [ ! -f .env ]; then \
		echo "Ошибка: файл .env не найден."; \
		echo "Создай его командой: cp .env.example .env"; \
		exit 1; \
	fi

check-docker: check-env ## Проверить Docker и конфигурацию Compose
	@command -v docker >/dev/null 2>&1 || { \
		echo "Ошибка: Docker не найден."; exit 1; \
	}
	@docker compose version >/dev/null
	@test -f docker/docker-compose.yml || { \
		echo "Ошибка: docker/docker-compose.yml не найден."; exit 1; \
	}
	@$(COMPOSE) config -q

# ─── Docker Compose ──────────────────────────────────────────────────────────

up: check-docker ## Запустить весь стек и дождаться healthy
	$(COMPOSE) up -d --build --wait

up-no-wait: check-docker ## Запустить весь стек без ожидания healthy
	$(COMPOSE) up -d --build

up-infra: check-docker ## Запустить только инфраструктуру (postgres, minio, rabbitmq)
	$(COMPOSE) up -d --wait --wait-timeout 120 postgres minio rabbitmq

down: check-docker ## Остановить контейнеры, сохранив данные
	$(COMPOSE) down

down-v: check-docker ## Остановить и удалить volumes (ОСТОРОЖНО)
	$(COMPOSE) down -v

restart: check-docker ## Перезапустить весь стек
	$(COMPOSE) restart

rebuild-app: check-docker ## Пересобрать и перезапустить только server и worker
	$(COMPOSE) up -d --build --wait server worker

build-images: check-docker ## Собрать Docker-образы server и worker
	$(COMPOSE) build server worker

ps: check-docker ## Статус контейнеров
	$(COMPOSE) ps

config: check-docker ## Проверить и вывести конфигурацию Compose
	$(COMPOSE) config

# ─── Logs ────────────────────────────────────────────────────────────────────

logs: check-docker ## Логи всех сервисов
	$(COMPOSE) logs -f

logs-server: check-docker ## Логи HTTP-сервера
	$(COMPOSE) logs -f server

logs-worker: check-docker ## Логи воркера
	$(COMPOSE) logs -f worker

logs-infra: check-docker ## Логи инфраструктуры
	$(COMPOSE) logs -f postgres minio rabbitmq

# ─── Go ──────────────────────────────────────────────────────────────────────

deps: ## Скачать зависимости
	$(GO) mod download

build: ## Проверить сборку всех пакетов
	$(GO) build ./...

build-binaries: ## Собрать бинарники в bin/
	mkdir -p bin
	$(GO) build -o bin/server ./cmd/server
	$(GO) build -o bin/worker ./cmd/worker

# ─── Tests & Quality ─────────────────────────────────────────────────────────

test: ## Запустить тесты
	$(GO) test ./... -count=1

test-v: ## Тесты с подробным выводом
	$(GO) test ./... -count=1 -v

test-cover: ## Покрытие тестами
	$(GO) test ./... -count=1 -coverprofile=cover.out
	$(GO) tool cover -func=cover.out | tail -1

test-race: ## Тесты с race detector
	$(GO) test ./... -count=1 -race

lint: ## Запустить golangci-lint
	golangci-lint run ./...

vet: ## Запустить go vet
	$(GO) vet ./...

fmt: ## Отформатировать Go-код
	gofmt -w $$(find . -type f -name '*.go' -not -path './vendor/*')

fmt-check: ## Проверить форматирование
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "Нужно отформатировать:"; echo "$$files"; exit 1; \
	fi

clean: ## Удалить бинарники и файлы покрытия
	rm -rf bin/
	rm -f cover.out cover.html
	$(GO) clean -testcache

clean-all: down-v clean ## Полная очистка (контейнеры + volumes + файлы)

# ─── Local run ───────────────────────────────────────────────────────────────

run-server: ## Запустить server локально (нужен make up-infra)
	$(GO) run ./cmd/server

run-worker: ## Запустить worker локально (нужен make up-infra)
	$(GO) run ./cmd/worker