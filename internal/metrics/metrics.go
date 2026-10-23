// Package metrics содержит Prometheus-метрики HTTP-уровня (RED: rate, errors,
// duration), бизнес-метрики аватарок и метрики пула PostgreSQL.
//
// Коллекторы собраны в структуру Set, а не в пакетные переменные: так
// зависимости видны явно, а в тестах можно создать отдельный реестр и не
// смешивать наблюдения разных тестов. Пакет не держит глобального состояния:
// набор метрик создаёт main (с prometheus.DefaultRegisterer) или тест
// (с prometheus.NewRegistry()) и передаёт его явно.
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

// Метки, общие для HTTP- и бизнес-метрик: метод, шаблон маршрута, код ответа
// или исход операции.
const (
	labelMethod = "method"
	labelPath   = "path"
	labelStatus = "status"
)

// Set — набор коллекторов одного процесса.
//
// Поля экспортированы, чтобы потребители могли писать наблюдения, но создавать
// коллекторы напрямую нельзя: это делает NewSet, который регистрирует их
// в переданном реестре.
type Set struct {
	reg      prometheus.Registerer
	gatherer prometheus.Gatherer

	HTTPRequestsTotal         *prometheus.CounterVec
	HTTPRequestDuration       *prometheus.HistogramVec
	AvatarsUploadsTotal       *prometheus.CounterVec
	AvatarsUploadDuration     *prometheus.HistogramVec
	AvatarsDeletesTotal       *prometheus.CounterVec
	AvatarsStorageBytes       prometheus.Gauge
	AvatarsFileSize           prometheus.Histogram
	AvatarsProcessingDuration *prometheus.HistogramVec
}

// NewSet создаёт коллекторы и регистрирует их в reg.
//
// reg обязателен: main передаёт prometheus.DefaultRegisterer, тесты — отдельный
// prometheus.NewRegistry(). Паника вместо «тихого» fallback на глобальный реестр
// нужна потому, что забытый аргумент иначе проявился бы только пустым /metrics.
//
// reg должен реализовывать и prometheus.Gatherer (это верно для *Registry и для
// реестра по умолчанию): Handler отдаёт именно его содержимое.
func NewSet(reg prometheus.Registerer) *Set {
	if reg == nil {
		panic("metrics: NewSet: registerer is nil")
	}

	gatherer, ok := reg.(prometheus.Gatherer)
	if !ok {
		panic("metrics: NewSet: registerer must also implement prometheus.Gatherer")
	}

	factory := promauto.With(reg)
	m := &Set{
		reg:      reg,
		gatherer: gatherer,
		HTTPRequestsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: MetricHTTPRequestsTotal,
				Help: "Total number of handled HTTP requests by method, route pattern and status code.",
			},
			[]string{labelMethod, labelPath, labelStatus},
		),
		HTTPRequestDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    MetricHTTPRequestDuration,
				Help:    "HTTP request duration in seconds by method, route pattern and status code.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{labelMethod, labelPath, labelStatus},
		),
		AvatarsUploadsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: MetricAvatarsUploadsTotal,
				Help: "Total number of avatar uploads by status.",
			},
			[]string{labelStatus},
		),
		AvatarsUploadDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    MetricAvatarsUploadDuration,
				Help:    "Avatar upload duration in seconds by status.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{labelStatus},
		),
		AvatarsDeletesTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: MetricAvatarsDeletesTotal,
				Help: "Total number of avatar deletions by status.",
			},
			[]string{labelStatus},
		),
		AvatarsStorageBytes: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: MetricAvatarsStorageBytes,
				Help: "Total size of active avatars in object storage, in bytes.",
			},
		),
		AvatarsFileSize: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    MetricAvatarsFileSize,
				Help:    "Avatar file size distribution in bytes.",
				Buckets: fileSizeBuckets,
			},
		),
		AvatarsProcessingDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    MetricAvatarsProcessingDuration,
				Help:    "Avatar processing duration in the worker, in seconds, by status.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{labelStatus},
		),
	}

	return m
}

// Handler отдаёт метрики этого набора в текстовом формате Prometheus.
//
// Отдаётся содержимое реестра, переданного в NewSet, — не глобального:
// у server и worker свои процессы и свои наборы.
func (m *Set) Handler() http.Handler {
	return promhttp.HandlerFor(m.gatherer, promhttp.HandlerOpts{})
}
