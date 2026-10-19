package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/api"
	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/handlers"
	"github.com/b602op/avatars-service/internal/metrics"
	"github.com/b602op/avatars-service/internal/migrator"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/pkg/logger"
	"github.com/b602op/avatars-service/pkg/telemetry"
)

const (
	// initTimeout — время на инициализацию подключений (БД, S3).
	initTimeout = 15 * time.Second
	// migrationTimeout — бюджет на применение миграций. Отдельно от initTimeout:
	// подключение к БД укладывается в секунды, а миграции могут создавать
	// индексы и выполняться заметно дольше.
	migrationTimeout = 60 * time.Second
	// shutdownTimeout — время на корректное завершение HTTP-сервера.
	shutdownTimeout = 30 * time.Second
	// idleTimeout — время удержания idle-соединений.
	idleTimeout = 60 * time.Second
	// readHeaderTimeout — защита от slowloris: клиент должен прислать заголовки
	// за это время, иначе соединение закрывается.
	readHeaderTimeout = 5 * time.Second
	// writeTimeout — верхняя граница времени записи ответа. С запасом больше
	// таймаута middleware (30s), чтобы большие файлы успели дойти до клиента.
	writeTimeout = 60 * time.Second

	// migrateOnlyEnv — переменная окружения, включающая режим одноразового
	// применения миграций. Используется Helm-хуком (Job gophprofile-migrate в
	// helm/gophprofile/templates/70-migration-job.yaml): тот запускает этот же
	// образ с MIGRATE_ONLY=true и ждёт успешного завершения.
	migrateOnlyEnv = "MIGRATE_ONLY"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// run возвращает именованный err, чтобы отложенные shutdown-функции могли
// присоединить свои ошибки к результату через errors.Join.
func run() (err error) {
	// .env необязателен: если файла нет — работаем с переменными окружения ОС.
	// Если файл есть, но битый — падаем громко с понятной ошибкой.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to load .env file: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	log := logger.New(cfg.LogLevel, "server")
	slog.SetDefault(log)

	log.Info("starting server", "config", cfg.String())

	// Регистрируем обработку сигналов до инициализации зависимостей.
	signalCtx, stopSignals := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stopSignals()

	// Режим миграций проверяется до телеметрии и сервера метрик: в этом режиме
	// процесс нужен только чтобы применить схему и завершиться, поэтому
	// трейсинг, /metrics и HTTP-сервер не поднимаются вообще.
	if os.Getenv(migrateOnlyEnv) == "true" {
		return migrateOnly(signalCtx, cfg, log)
	}

	// Трейсинг: OTLP HTTP → Jaeger. Недоступность коллектора не фатальна:
	// соединение устанавливается лениво, а ошибки экспорта уходят в наш slog
	// через otel.SetErrorHandler.
	tpShutdown, err := telemetry.Init(signalCtx, telemetry.Config{
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    cfg.OTelServiceName + "-server",
		ServiceVersion: cfg.OTelServiceVersion,
		Environment:    cfg.OTelEnvironment,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	// Трейсинг останавливается последним по времени: defer выполняется по LIFO,
	// а этот зарегистрирован раньше остальных, поэтому к моменту flush уже
	// остановлены HTTP-сервер, пул БД и publisher — их shutdown-спаны попадают
	// в батч. Порядок менять не нужно.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(
			context.WithoutCancel(signalCtx),
			telemetry.ShutdownTimeout,
		)
		defer cancelShutdown()

		// Трейсы — best-effort телеметрия: неудачный flush не должен
		// менять код возврата и провоцировать рестарт контейнера.
		if shutdownErr := tpShutdown(shutdownCtx); shutdownErr != nil {
			log.Warn("telemetry shutdown failed", "error", shutdownErr)
		}
	}()

	// /metrics живёт на отдельном порту: в основной Chi-роутер его не добавляем,
	// иначе метрики стали бы доступны через публичный API. Падение этого сервера
	// не должно ронять API — это вспомогательный сигнал, поэтому только логируем.
	metricsSrv := api.NewMetricsServer(cfg.MetricsAddr, log, false)
	go func() {
		log.Info("metrics server listening", "addr", metricsSrv.Addr)
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("metrics server failed", "error", serveErr)
		}
	}()
	// Регистрируется после трейсинга: defer выполняется по LIFO, поэтому
	// метрики останавливаются раньше, чем досылается батч спанов.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(
			context.WithoutCancel(signalCtx),
			shutdownTimeout,
		)
		defer cancelShutdown()

		if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("shutdown metrics server: %w", shutdownErr))
		}
	}()

	// Контекст только для инициализации зависимостей.
	initCtx, cancelInit := context.WithTimeout(context.Background(), initTimeout)
	defer cancelInit()

	// PostgreSQL.
	pool, err := postgres.NewPool(initCtx, cfg.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %w", err)
	}
	defer pool.Close()

	// Метрики пула отдаются вместе с HTTP-метриками на /metrics.
	metrics.RegisterDBPoolMetrics(pool)

	// avatars_storage_bytes: первый пересчёт сразу, дальше раз в 30 секунд.
	// Горутина завершается по signalCtx при остановке процесса.
	go metrics.StartStorageUpdater(signalCtx, pool, log)

	log.Info("postgres connected")

	// Миграции применяются при старте
	// повторный запуск и параллельный старт server/worker безопасны.
	if err := applyMigrations(signalCtx, pool, log); err != nil {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}

	log.Info("migrations applied")

	// S3-хранилище: клиент создаётся всегда, бакет гарантируется идемпотентно.
	storage, err := s3.New(cfg.S3, log)
	if err != nil {
		return fmt.Errorf("failed to create s3 client: %w", err)
	}

	if err := storage.EnsureBucket(initCtx); err != nil {
		return fmt.Errorf("failed to ensure s3 bucket: %w", err)
	}

	log.Info("s3 bucket ready", "bucket", cfg.S3.Bucket)

	// Паблишер событий: реальный RabbitMQ.
	pub, err := publisher.NewRabbitMQ(cfg.RabbitMQ, log)
	if err != nil {
		return fmt.Errorf("failed to create publisher: %w", err)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			log.Error("failed to close RabbitMQ publisher", "error", err)
		}
	}()

	// Инициализация закончена — дальнейшая работа не должна зависеть
	// от таймаута, установленного для старта приложения.
	cancelInit()

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
		AllowedOrigins: []string{"*"},
		RateLimitRPS:   cfg.HTTP.RateLimitRPS,
		RateLimitBurst: cfg.HTTP.RateLimitBurst,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           router,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	return serveHTTP(signalCtx, srv, log)
}

// migrateOnly применяет миграции и завершается, не поднимая HTTP-сервер,
// сервер метрик и трейсинг. Нужен Helm-хуку: Job gophprofile-migrate запускает
// образ server с MIGRATE_ONLY=true и считает деплой успешным только если схема
// применена. Пула хватает одного соединения, поэтому отдельный «лёгкий» путь
// к БД не нужен — используется тот же postgres.NewPool.
func migrateOnly(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("MIGRATE_ONLY is set: applying migrations and exiting")

	// Пул создаётся с таймаутом инициализации (подключение + ping), а сами
	// миграции получают собственный бюджет — см. applyMigrations.
	initCtx, cancelInit := context.WithTimeout(context.Background(), initTimeout)
	defer cancelInit()

	pool, err := postgres.NewPool(initCtx, cfg.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("migrate only: connect to postgres: %w", err)
	}
	defer pool.Close()

	if err := applyMigrations(ctx, pool, log); err != nil {
		return fmt.Errorf("migrate only: %w", err)
	}

	log.Info("migrations applied, exiting (MIGRATE_ONLY)")

	return nil
}

// applyMigrations применяет миграции с отдельным бюджетом времени: подключение
// к БД укладывается в секунды, а миграции могут создавать индексы и выполняться
// заметно дольше, поэтому initTimeout для них не подходит.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	migCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	return migrator.Up(migCtx, pool, log)
}

func serveHTTP(
	ctx context.Context,
	srv *http.Server,
	log *slog.Logger,
) error {
	// Буфер позволяет горутине сообщить результат Serve
	serveErr := make(chan error, 1)

	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// До сигнала остановки сервер не должен завершаться сам.
		if err == nil {
			return errors.New("http server stopped unexpectedly")
		}
		return fmt.Errorf("http server stopped before shutdown: %w", err)

	case <-ctx.Done():
		log.Info("shutting down http server")
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		shutdownTimeout,
	)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown не успел дождаться активных запросов.
		// Принудительно закрываем соединения и дожидаемся завершения
		// горутины Serve, прежде чем run() начнёт освобождать зависимости.
		log.Error("graceful shutdown failed; forcing close", "error", err)

		closeErr := srv.Close()
		serveErr := <-serveErr

		return errors.Join(
			fmt.Errorf("shutdown http server: %w", err),
			wrapCloseError(closeErr),
			unexpectedServeError(serveErr),
		)
	}

	// Shutdown закрывает listener, поэтому Serve должен завершиться
	// с http.ErrServerClosed. Дожидаемся результата горутины.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server failed during shutdown: %w", err)
	}

	log.Info("http server stopped gracefully")

	return nil
}

func wrapCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("force close http server: %w", err)
}

func unexpectedServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("http server serve error: %w", err)
}
