package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/api"
	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/handlers"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/pkg/logger"
)

const (
	// initTimeout — время на инициализацию подключений (БД, S3).
	initTimeout = 15 * time.Second
	// shutdownTimeout — время на корректное завершение HTTP-сервера.
	shutdownTimeout = 30 * time.Second
	// idleTimeout — время удержания idle-соединений.
	idleTimeout = 60 * time.Second
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

	// Контекст только для инициализации зависимостей.
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()

	// PostgreSQL.
	pool, err := postgres.NewPool(ctx, cfg.Postgres.DSN())
	if err != nil {
		log.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	log.Info("postgres connected")

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

	// Паблишер событий: реальный RabbitMQ.
	pub, err := publisher.NewRabbitMQ(cfg.RabbitMQ, log)
	if err != nil {
		log.Error("failed to create publisher", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			log.Error("failed to close publisher", "error", err)
		}
	}()

	// Сборка слоёв: репозиторий → сервис → хендлер → роутер.
	repo := postgres.NewAvatarRepository(pool)
	svc := services.NewAvatarService(repo, storage, pub, log)
	avatarHandler := handlers.NewAvatarHandler(svc, log)

	// /health проверяет все внешние зависимости; собирается здесь, потому что
	// уровню api (роутеру) pool, storage и брокер недоступны.
	healthHandler := handlers.Health(pool, storage, pub.Conn(), log)

	router := api.NewRouter(api.RouterDeps{
		Log:            log,
		AvatarHandler:  avatarHandler,
		Health:         healthHandler,
		AllowedOrigins: []string{"*"}, // MVP: разрешаем все источники
	})

	srv := &http.Server{
		Addr:         ":" + cfg.HTTP.Port,
		Handler:      router,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  idleTimeout,
	}

	// Запуск сервера в отдельной горутине, чтобы main мог ловить сигналы.
	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Ожидание сигнала завершения.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("server stopped gracefully")
}
