// Package metrics содержит Prometheus-метрики HTTP-уровня (RED: rate, errors,
// duration) и хендлер /metrics.
//
// Метрики регистрируются в глобальном реестре через promauto: пакет один на
// процесс, поэтому повторной регистрации (и паники) не происходит. У server и
// worker реестры свои — это разные процессы.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Имена метрик вынесены в константы, чтобы код, тесты и дашборды не расходились.
const (
	// MetricHTTPRequestsTotal — счётчик запросов (rate и errors в RED).
	MetricHTTPRequestsTotal = "http_requests_total"
	// MetricHTTPRequestDuration — гистограмма длительности (duration в RED).
	MetricHTTPRequestDuration = "http_request_duration_seconds"
)

// Метки, общие для обеих метрик: метод, шаблон маршрута и код ответа.
const (
	labelMethod = "method"
	labelPath   = "path"
	labelStatus = "status"
)

// HTTP-метрики уровня приложения.
var (
	// HTTPRequestsTotal — количество обработанных HTTP-запросов.
	//
	// path — шаблон маршрута (chi RoutePattern), а не конкретный URL: иначе
	// каждая аватарка создавала бы новую временную серию и кардинальность
	// метки росла бы вместе с данными.
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricHTTPRequestsTotal,
			Help: "Total number of handled HTTP requests by method, route pattern and status code.",
		},
		[]string{labelMethod, labelPath, labelStatus},
	)

	// HTTPRequestDuration — длительность обработки HTTP-запросов в секундах.
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    MetricHTTPRequestDuration,
			Help:    "HTTP request duration in seconds by method, route pattern and status code.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelMethod, labelPath, labelStatus},
	)
)

// Handler отдаёт собранные метрики в текстовом формате Prometheus.
func Handler() http.Handler {
	return promhttp.Handler()
}
