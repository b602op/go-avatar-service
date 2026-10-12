package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/services/s3"
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
	log.Info("starting server", "config", cfg.String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// S3-хранилище: клиент создаётся всегда, бакет гарантируется идемпотентно.
	storage, err := s3.New(cfg.S3, log)
	if err != nil {
		log.Error("failed to create s3 client", "error", err)
		os.Exit(1)
	}
	if err := storage.EnsureBucket(ctx); err != nil {
		log.Error("failed to ensure s3 bucket", "error", err)
		os.Exit(1)
	}
	log.Info("s3 bucket ready", "bucket", cfg.S3.Bucket)

	// HTTP-сервер и роутер появятся на ШАГЕ 6.
	os.Exit(0)
}
