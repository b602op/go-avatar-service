// Package metrics содержит Prometheus-метрики HTTP-уровня (RED: rate, errors,
// duration), бизнес-метрики аватарок и метрики пула PostgreSQL.
//
// Коллекторы собраны в структуру Metrics, а не в пакетные переменные: так
// зависимости видны явно, а в тестах можно создать отдельный реестр и не
// смешивать наблюдения разных тестов. Для production есть Default(), который
// регистрирует метрики в реестре Prometheus по умолчанию.
package metrics

import (
	"net/http"
	"sync"

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

// Metrics — набор коллекторов одного процесса.
//
// Поля экспортированы, чтобы потребители могли писать наблюдения, но создавать
// коллекторы напрямую нельзя: это делает NewMetrics, который регистрирует их
// в переданном реестре.
type Metrics struct {
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

var (
	defaultOnce    sync.Once
	defaultMetrics *Metrics
)

// NewMetrics создаёт коллекторы и регистрирует их в reg.
//
// reg может быть как *prometheus.Registry (тогда Handler отдаёт именно его
// содержимое), так и любым другим Registerer. nil означает реестр по умолчанию.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	factory := promauto.With(reg)
	m := &Metrics{
		reg: reg,
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

	if gatherer, ok := reg.(prometheus.Gatherer); ok {
		m.gatherer = gatherer
	}

	return m
}

// Default возвращает метрики, зарегистрированные в реестре по умолчанию.
//
// Создаётся один раз на процесс: повторная регистрация тех же коллекторов в
// default registry привела бы к панике. Основной способ получить метрики —
// передать NewMetrics(...) явно; Default нужен как безопасный fallback для
// конструкторов, которые метрики не получили.
func Default() *Metrics {
	defaultOnce.Do(func() {
		defaultMetrics = NewMetrics(prometheus.DefaultRegisterer)
	})
	return defaultMetrics
}

// Handler отдаёт метрики этого набора в текстовом формате Prometheus.
//
// Если NewMetrics получил *prometheus.Registry, отдаётся его содержимое; иначе
// используется реестр по умолчанию.
func (m *Metrics) Handler() http.Handler {
	if m != nil && m.gatherer != nil {
		return promhttp.HandlerFor(m.gatherer, promhttp.HandlerOpts{})
	}
	return promhttp.Handler()
}
