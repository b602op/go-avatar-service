package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/b602op/avatars-service/internal/domain"
)

// healthTimeout — верхняя граница на все проверки компонентов.
//
// Проверки идут последовательно и делят один контекст, поэтому общее время
// ответа /health ограничено этим значением даже при «зависшей» зависимости.
const healthTimeout = 3 * time.Second

// Статус отдельного компонента.
const (
	componentOK   = "ok"
	componentFail = "fail"
)

// Статус сервиса целиком.
const (
	statusOK       = "ok"       // все компоненты живы
	statusDegraded = "degraded" // часть компонентов недоступна
	statusDown     = "down"     // недоступны все компоненты
)

// Имена компонентов в ответе.
const (
	componentDB     = "db"
	componentS3     = "s3"
	componentBroker = "broker"
)

// DBHealthChecker — проверка доступности БД.
//
// Интерфейс объявлен на стороне потребителя: это позволяет подменять пул в
// тестах, не поднимая реальный PostgreSQL. *pgxpool.Pool ему удовлетворяет.
type DBHealthChecker interface {
	Ping(ctx context.Context) error
}

// HealthChecker — проверка «живости» брокера.
//
// *amqp.Connection удовлетворяет интерфейсу (IsClosed).
type HealthChecker interface {
	IsClosed() bool
}

// Проверки на этапе компиляции, что реальные зависимости подходят под интерфейсы.
var (
	_ DBHealthChecker = (*pgxpool.Pool)(nil)
	_ HealthChecker   = (*amqp.Connection)(nil)
)

// ComponentStatus — состояние одного компонента.
type ComponentStatus struct {
	Status string `json:"status"`          // "ok" | "fail"
	Error  string `json:"error,omitempty"` // причина отказа
}

// HealthResponse — тело ответа /health.
type HealthResponse struct {
	Status     string                     `json:"status"` // "ok" | "degraded" | "down"
	Components map[string]ComponentStatus `json:"components"`
}

// Health отдаёт состояние сервиса и его зависимостей.
//
// Компоненты проверяются независимо: отказ одного не мешает проверить
// остальные, поэтому в ответе видно, что именно сломалось.
//
// HTTP-код: 200 при полном успехе, 503 в остальных случаях — так балансировщик
// или оркестратор выводит инстанс из ротации, не дожидаясь таймаута запроса.
func Health(
	pool DBHealthChecker,
	storage domain.ObjectStorage,
	broker HealthChecker,
	log *slog.Logger,
) http.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		components := map[string]ComponentStatus{
			componentDB:     checkDB(ctx, pool),
			componentS3:     checkS3(ctx, storage),
			componentBroker: checkBroker(broker),
		}

		status, code := overallStatus(components)
		if code != http.StatusOK {
			// Warn, а не Error: сервис жив и отвечает, но деградировал.
			log.Warn("health check failed", "status", status, "components", components)
		}

		respondJSON(w, code, HealthResponse{Status: status, Components: components})
	}
}

// liveResponse — тело ответа /live.
type liveResponse struct {
	Status string `json:"status"`
}

// Live отдаёт 200, если процесс жив и обслуживает HTTP.
//
// Зависимости здесь НЕ проверяются намеренно — в этом весь смысл эндпоинта.
// /health отвечает на вопрос «можно ли направлять сюда трафик» (и потому
// опрашивает БД, S3 и брокер), а /live — на вопрос «нужно ли перезапускать
// процесс». Смешивать их нельзя: если liveness-проба опрашивает зависимость,
// её падение превращается в перезапуск пода, который ничего не лечит — процесс
// жив, а после рестарта приложение падает на старте (EnsureBucket идёт в
// недоступный S3) и уходит в CrashLoopBackOff.
//
// По той же причине у /live нет ни таймаута, ни внешних вызовов: ответ обязан
// быть мгновенным и безусловным, иначе проба сама станет источником ложных
// перезапусков.
func Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"` + statusOK + `"}`))
}

// overallStatus сводит состояния компонентов к общему статусу и HTTP-коду.
func overallStatus(components map[string]ComponentStatus) (string, int) {
	okCount := 0
	for _, c := range components {
		if c.Status == componentOK {
			okCount++
		}
	}

	switch {
	case okCount == len(components):
		return statusOK, http.StatusOK
	case okCount > 0:
		return statusDegraded, http.StatusServiceUnavailable
	default:
		return statusDown, http.StatusServiceUnavailable
	}
}

// checkDB проверяет соединение с БД.
func checkDB(ctx context.Context, pool DBHealthChecker) ComponentStatus {
	if pool == nil {
		return failed("pool is nil")
	}
	if err := pool.Ping(ctx); err != nil {
		return failed(err.Error())
	}
	return ComponentStatus{Status: componentOK}
}

// checkS3 проверяет доступность хранилища и наличие бакета.
func checkS3(ctx context.Context, storage domain.ObjectStorage) ComponentStatus {
	if storage == nil {
		return failed("storage is nil")
	}
	if err := storage.HealthCheck(ctx); err != nil {
		return failed(err.Error())
	}
	return ComponentStatus{Status: componentOK}
}

// checkBroker проверяет, что соединение с брокером не закрыто.
func checkBroker(broker HealthChecker) ComponentStatus {
	if broker == nil {
		return failed("broker is nil")
	}
	if broker.IsClosed() {
		return failed("connection closed")
	}
	return ComponentStatus{Status: componentOK}
}

// failed собирает статус отказа с причиной.
func failed(reason string) ComponentStatus {
	return ComponentStatus{Status: componentFail, Error: reason}
}
