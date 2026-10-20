package metrics

import (
	"context"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// histogramSampleCount возвращает число наблюдений гистограммы с заданной меткой
// status: testutil.ToFloat64 гистограммы не поддерживает, а счётчик наблюдений —
// это ровно то, что нужно проверять в тестах.
func histogramSampleCount(t *testing.T, gatherer prometheus.Gatherer, name, status string) uint64 {
	t.Helper()

	families, err := gatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelStatus && label.GetValue() == status {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}

	t.Fatalf("гистограмма %s{status=%q} не найдена", name, status)

	return 0
}

// collectValue вытаскивает единственную метрику коллектора и возвращает её значение.
func collectValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	close(ch)

	metric, ok := <-ch
	require.True(t, ok, "коллектор не отдал ни одной метрики")

	pb := &dto.Metric{}
	require.NoError(t, metric.Write(pb))

	switch {
	case pb.Gauge != nil:
		return pb.GetGauge().GetValue()
	case pb.Counter != nil:
		return pb.GetCounter().GetValue()
	default:
		t.Fatal("ожидались gauge или counter")
		return 0
	}
}

// TestSizeClass проверяет границы категорий размера: они нужны только для логов,
// но именно по ним в Loki ищут «крупные загрузки».
func TestSizeClass(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"empty file", 0, "small"},
		{"just below 100 KB", 100*1024 - 1, "small"},
		{"100 KB boundary", 100 * 1024, "medium"},
		{"just below 1 MB", 1024*1024 - 1, "medium"},
		{"1 MB boundary", 1024 * 1024, "large"},
		{"10 MB", 10 * 1024 * 1024, "large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SizeClass(tt.bytes))
		})
	}
}

// TestFileSizeBuckets проверяет границы гистограммы: последняя граница — +Inf,
// иначе наблюдения больше 50 МБ попали бы не в тот бакет.
func TestFileSizeBuckets(t *testing.T) {
	require.NotEmpty(t, fileSizeBuckets)

	for i := 1; i < len(fileSizeBuckets); i++ {
		assert.Less(t, fileSizeBuckets[i-1], fileSizeBuckets[i], "границы должны возрастать")
	}

	assert.Equal(t, 1024.0, fileSizeBuckets[0])
	assert.True(t, math.IsInf(fileSizeBuckets[len(fileSizeBuckets)-1], 1),
		"последняя граница должна быть +Inf")
}

// TestBusinessMetricsRegistered проверяет, что метрики зарегистрированы в
// глобальном реестре под ожидаемыми именами и у них нет лейбла user_id
// (кардинальность не должна расти вместе с числом пользователей).
func TestBusinessMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewSet(reg)

	// Векторные метрики появляются в выдаче только после первой записи.
	m.AvatarsUploadsTotal.WithLabelValues(StatusSuccess).Inc()
	m.AvatarsDeletesTotal.WithLabelValues(StatusSuccess).Inc()
	m.AvatarsUploadDuration.WithLabelValues(StatusSuccess).Observe(0.1)
	m.AvatarsProcessingDuration.WithLabelValues(StatusSuccess).Observe(0.1)
	m.AvatarsFileSize.Observe(2048)
	m.AvatarsStorageBytes.Set(0)

	families, err := reg.Gather()
	require.NoError(t, err)

	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}

	wantNames := []string{
		MetricAvatarsUploadsTotal,
		MetricAvatarsUploadDuration,
		MetricAvatarsDeletesTotal,
		MetricAvatarsStorageBytes,
		MetricAvatarsFileSize,
		MetricAvatarsProcessingDuration,
	}
	for _, name := range wantNames {
		assert.Contains(t, byName, name, "метрика %s не зарегистрирована", name)
	}

	// Проверяем метки у семейств, у которых они есть: только status.
	for _, name := range []string{
		MetricAvatarsUploadsTotal,
		MetricAvatarsUploadDuration,
		MetricAvatarsDeletesTotal,
		MetricAvatarsProcessingDuration,
	} {
		family := byName[name]
		require.NotEmpty(t, family.GetMetric())

		for _, label := range family.GetMetric()[0].GetLabel() {
			assert.Equal(t, "status", label.GetName(),
				"у метрики %s ожидается единственная метка status", name)
		}
	}

	// avatars_storage_bytes — одна серия без лейблов.
	storageFamily := byName[MetricAvatarsStorageBytes]
	require.Len(t, storageFamily.GetMetric(), 1)
	assert.Empty(t, storageFamily.GetMetric()[0].GetLabel())
}

// TestBusinessMetricsRecordObservations проверяет запись значений в метрики:
// счётчик инкрементится, гистограмма накапливает наблюдения.
func TestBusinessMetricsRecordObservations(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewSet(reg)

	uploads := m.AvatarsUploadsTotal.WithLabelValues(StatusSuccess)
	// HistogramVec появляется в Gather только после создания серии; без этого
	// первый замер до наблюдения не нашёл бы гистограмму.
	m.AvatarsUploadDuration.WithLabelValues(StatusSuccess)

	uploadsBefore := collectValue(t, uploads)
	durationBefore := histogramSampleCount(t, reg, MetricAvatarsUploadDuration, StatusSuccess)

	uploads.Inc()
	m.AvatarsUploadDuration.WithLabelValues(StatusSuccess).Observe(0.25)

	assert.Equal(t, uploadsBefore+1, collectValue(t, uploads))
	assert.Equal(t, durationBefore+1, histogramSampleCount(t, reg, MetricAvatarsUploadDuration, StatusSuccess))
}

// TestStartStorageUpdater_StopsOnContextCancel проверяет, что фоновая горутина
// не переживает отмену контекста: иначе она осталась бы висеть после shutdown.
//
// Пул создаётся ленивым (DSN на закрытый порт): запрос гарантированно падает,
// метрика не выставляется, но процесс не падает и горутина завершается.
func TestStartStorageUpdater_StopsOnContextCancel(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://avatar:avatar@127.0.0.1:1/avatars?sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithCancel(t.Context())

	m := NewSet(prometheus.NewRegistry())

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.StartStorageUpdater(ctx, pool, slog.New(slog.DiscardHandler))
	}()

	// Первый пересчёт выполняется сразу: даём горутине дойти до цикла.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartStorageUpdater не завершился после отмены контекста")
	}
}
