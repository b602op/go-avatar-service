package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/metrics"
	"github.com/b602op/avatars-service/internal/services/publisher"
)

// processingSamples возвращает число наблюдений avatars_processing_duration_seconds
// с заданной меткой status. Читается из реестра теста, а не из глобального:
// пакет metrics больше не регистрирует ничего «по умолчанию».
func processingSamples(t *testing.T, reg *prometheus.Registry, status string) uint64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != metrics.MetricAvatarsProcessingDuration {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "status" && label.GetValue() == status {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}

	return 0
}

// metricsRepo — репозиторий-стаб для проверки метрик обработки: поведение
// задаётся функцией, паника внутри неё имитирует сбой на любом шаге.
func metricsRepo(fn func(ctx context.Context, id uuid.UUID) (*domain.Avatar, error)) *stubRepo {
	return &stubRepo{getByIDFn: fn}
}

// TestProcessingDurationMetrics проверяет метку исхода: успех, ошибка и паника
// должны попадать в разные серии — иначе падение воркера выглядело бы как
// обычная ошибка обработки.
func TestProcessingDurationMetrics(t *testing.T) {
	event := publisher.AvatarUploadEvent{AvatarID: uuid.NewString(), UserID: "user-1", S3Key: "originals/user-1/x/photo.jpg"}

	t.Run("success", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := metrics.NewSet(reg)
		before := processingSamples(t, reg, metrics.StatusSuccess)

		// Аватар уже обработан — обработчик выходит успешно, ничего не делая.
		repo := metricsRepo(func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return &domain.Avatar{ProcessingStatus: domain.ProcessingStatusCompleted}, nil
		})
		h := NewHandler(repo, stubStorage{}, &mockResizer{}, testLogger(), m)

		require.NoError(t, h.HandleUpload(context.Background(), event))

		assert.Equal(t, before+1, processingSamples(t, reg, metrics.StatusSuccess))
	})

	t.Run("error", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := metrics.NewSet(reg)
		before := processingSamples(t, reg, metrics.StatusError)

		repo := metricsRepo(func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return nil, errors.New("db down")
		})
		h := NewHandler(repo, stubStorage{}, &mockResizer{}, testLogger(), m)

		require.Error(t, h.HandleUpload(context.Background(), event))

		assert.Equal(t, before+1, processingSamples(t, reg, metrics.StatusError))
	})

	t.Run("panic", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := metrics.NewSet(reg)
		before := processingSamples(t, reg, metrics.StatusPanic)

		repo := metricsRepo(func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			panic("boom")
		})
		h := NewHandler(repo, stubStorage{}, &mockResizer{}, testLogger(), m)

		// Паника наблюдается метрикой и перебрасывается дальше.
		require.Panics(t, func() {
			_ = h.HandleUpload(context.Background(), event)
		})

		assert.Equal(t, before+1, processingSamples(t, reg, metrics.StatusPanic))
	})
}

// TestProcessingDurationMetrics_NotRecordedOnSoftSkip — «мягкие» случаи
// (аватар удалён) не являются ошибкой: обработка завершилась успешно.
func TestProcessingDurationMetrics_NotRecordedOnSoftSkip(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSet(reg)

	beforeSuccess := processingSamples(t, reg, metrics.StatusSuccess)
	beforeError := processingSamples(t, reg, metrics.StatusError)

	repo := metricsRepo(func(context.Context, uuid.UUID) (*domain.Avatar, error) {
		return nil, domain.ErrNotFound
	})
	h := NewHandler(repo, stubStorage{}, &mockResizer{}, testLogger(), m)

	event := publisher.AvatarUploadEvent{AvatarID: uuid.NewString(), UserID: "user-1"}
	require.NoError(t, h.HandleUpload(context.Background(), event))

	assert.Equal(t, beforeSuccess+1, processingSamples(t, reg, metrics.StatusSuccess))
	assert.Equal(t, beforeError, processingSamples(t, reg, metrics.StatusError))
}
