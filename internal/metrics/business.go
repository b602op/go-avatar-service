package metrics

import (
	"math"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Имена бизнес-метрик вынесены в константы, чтобы код, тесты и дашборды
// не расходились.
const (
	MetricAvatarsUploadsTotal       = "avatars_uploads_total"
	MetricAvatarsUploadDuration     = "avatars_upload_duration_seconds"
	MetricAvatarsDeletesTotal       = "avatars_deletes_total"
	MetricAvatarsStorageBytes       = "avatars_storage_bytes"
	MetricAvatarsFileSize           = "avatars_file_size_bytes"
	MetricAvatarsProcessingDuration = "avatars_processing_duration_seconds"
)

// Значения метки labelStatus (сама метка объявлена в metrics.go: значение
// "status" одно и то же и для HTTP-кода, и для исхода бизнес-операции).
const (
	StatusSuccess = "success"
	StatusError   = "error"
	StatusPanic   = "panic"
)

// Размеры бакетов гистограммы avatars_file_size_bytes: от 1 КБ до 50 МБ.
// Явные границы нужны, потому что дефолтные бакеты Prometheus рассчитаны на
// секунды, а не на байты.
var fileSizeBuckets = []float64{
	1024,             // 1 KB
	10 * 1024,        // 10 KB
	100 * 1024,       // 100 KB
	1024 * 1024,      // 1 MB
	5 * 1024 * 1024,  // 5 MB
	10 * 1024 * 1024, // 10 MB
	50 * 1024 * 1024, // 50 MB
	math.Inf(1),      // +Inf (явно, чтобы верхняя граница читалась в коде)
}

// Бизнес-метрики аватарок.
//
// user_id сознательно НЕ выносится в лейблы: он уникален для каждого
// пользователя, и кардинальность метрики росла бы вместе с числом клиентов.
// Персональные атрибуты живут в логах (Loki) и спанах (Jaeger), а в
// Prometheus остаются только агрегаты по исходу операции.
var (
	// AvatarsUploadsTotal — количество загрузок аватарок по исходу операции.
	AvatarsUploadsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricAvatarsUploadsTotal,
			Help: "Total number of avatar uploads by status.",
		},
		[]string{labelStatus},
	)

	// AvatarsUploadDuration — длительность обработки загрузки: от входа в
	// сервис до ответа клиенту (валидация + S3 + БД + публикация события).
	AvatarsUploadDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    MetricAvatarsUploadDuration,
			Help:    "Avatar upload duration in seconds by status.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelStatus},
	)

	// AvatarsDeletesTotal — количество удалений аватарок по исходу операции.
	AvatarsDeletesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricAvatarsDeletesTotal,
			Help: "Total number of avatar deletions by status.",
		},
		[]string{labelStatus},
	)

	// AvatarsStorageBytes — суммарный размер активных аватарок в байтах.
	//
	// Значение выставляется фоновым обновлением (StartStorageUpdater), а не
	// вычисляется на скрейпе: агрегат требует запроса в БД, а /metrics не
	// должен зависеть от её доступности.
	AvatarsStorageBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: MetricAvatarsStorageBytes,
			Help: "Total size of active avatars in object storage, in bytes.",
		},
	)

	// AvatarsFileSize — распределение размеров загруженных файлов.
	//
	// Лейбла size_class здесь нет намеренно: дополнительная метка разбила бы
	// гистограмму на три независимые, и histogram_quantile по общей картине
	// перестал бы работать «из коробки». Категория small/medium/large живёт
	// в логах (см. SizeClass).
	AvatarsFileSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    MetricAvatarsFileSize,
			Help:    "Avatar file size distribution in bytes.",
			Buckets: fileSizeBuckets,
		},
	)

	// AvatarsProcessingDuration — длительность обработки аватарки воркером
	// (скачивание оригинала, ресайз, загрузка миниатюр, обновление статуса).
	AvatarsProcessingDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    MetricAvatarsProcessingDuration,
			Help:    "Avatar processing duration in the worker, in seconds, by status.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelStatus},
	)
)

// SizeClass возвращает категорию размера файла для логов.
//
// В метрики категория НЕ попадает: там для этого есть бакеты гистограммы
// AvatarsFileSize (см. комментарий к ней).
func SizeClass(bytes int64) string {
	switch {
	case bytes < 100*1024:
		return "small"
	case bytes < 1024*1024:
		return "medium"
	default:
		return "large"
	}
}
