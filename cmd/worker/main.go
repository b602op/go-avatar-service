package main

import (
	"log/slog"
	"os"

	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/pkg/logger"
)

func main() {
	// .env необязателен: если файла нет — работаем с переменными окружения ОС.
	// Если файл есть, но битый — падаем громко с понятной ошибкой.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to load .env file", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	log := logger.New(cfg.App.LogLevel)
	log.Info("starting worker", "config", cfg.String())

	// Подключение к RabbitMQ и обработка задач появятся на следующих шагах.
	os.Exit(0)
}
