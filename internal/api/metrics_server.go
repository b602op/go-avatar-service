package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/b602op/avatars-service/internal/metrics"
)

// metricsReadHeaderTimeout — защита от slowloris на вспомогательном сервере:
// заголовки запроса должны прийти за это время, иначе соединение закрывается.
const metricsReadHeaderTimeout = 5 * time.Second

// NewMetricsServer собирает отдельный HTTP-сервер для /metrics.
//
// Метрики сознательно НЕ добавляются в основной Chi-роутер: там они оказались
// бы доступны через публичный порт API. Отдельный порт (METRICS_ADDR) слушают
// только внутренние скрейперы Prometheus.
//
// withHealth добавляет /health — он нужен воркеру: своего HTTP-сервера у него
// нет, а healthcheck контейнера должен что-то опрашивать.
//
// metricsSet позволяет отдать метрики из конкретного реестра; без аргумента
// используется metrics.Default().
//
// log используется как ErrorLog сервера: без этого net/http писал бы свои
// ошибки в stderr мимо нашего JSON-логгера.
func NewMetricsServer(addr string, log *slog.Logger, withHealth bool, sets ...*metrics.Metrics) *http.Server {
	if log == nil {
		log = slog.Default()
	}

	metricsSet := metrics.Default()
	if len(sets) > 0 && sets[0] != nil {
		metricsSet = sets[0]
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsSet.Handler())
	if withHealth {
		mux.HandleFunc("/health", healthOK)
	}

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// healthOK — минимальный liveness-ответ воркера: он не проверяет зависимости,
// а лишь подтверждает, что процесс жив и обслуживает HTTP.
func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
