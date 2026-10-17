package services

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/metrics"
)

// counterValue возвращает текущее значение счётчика.
func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	close(ch)

	metric, ok := <-ch
	require.True(t, ok, "счётчик не отдал ни одной метрики")

	pb := &dto.Metric{}
	require.NoError(t, metric.Write(pb))

	return pb.GetCounter().GetValue()
}

// TestUploadMetrics — метрики загрузки выставляются по факту выхода из метода:
// именно поэтому Upload возвращает именованные значения, и ошибка на любом
// шаге (валидация, S3, БД) должна попадать в status="error".
func TestUploadMetrics(t *testing.T) {
	success := metrics.AvatarsUploadsTotal.WithLabelValues(metrics.StatusSuccess)
	failed := metrics.AvatarsUploadsTotal.WithLabelValues(metrics.StatusError)

	t.Run("success", func(t *testing.T) {
		before := counterValue(t, success)

		svc := NewAvatarService(&stubAvatarRepo{}, &stubObjectStorage{}, &stubEventPublisher{}, nil)
		_, err := svc.Upload(context.Background(), "user-1", "photo.jpg", mimeJPEG, jpegData)
		require.NoError(t, err)

		assert.Equal(t, before+1, counterValue(t, success))
	})

	t.Run("validation error", func(t *testing.T) {
		before := counterValue(t, failed)

		svc := NewAvatarService(&stubAvatarRepo{}, &stubObjectStorage{}, &stubEventPublisher{}, nil)
		_, err := svc.Upload(context.Background(), "user-1", "bad.txt", "text/plain", []byte("hello"))
		require.Error(t, err)

		assert.Equal(t, before+1, counterValue(t, failed))
	})

	t.Run("storage error", func(t *testing.T) {
		before := counterValue(t, failed)

		storage := &stubObjectStorage{
			uploadFn: func(context.Context, string, io.Reader, int64, string) error {
				return errors.New("s3 down")
			},
		}
		svc := NewAvatarService(&stubAvatarRepo{}, storage, &stubEventPublisher{}, nil)
		_, err := svc.Upload(context.Background(), "user-1", "photo.jpg", mimeJPEG, jpegData)
		require.Error(t, err)

		assert.Equal(t, before+1, counterValue(t, failed))
	})
}

// TestDeleteMetrics проверяет, что удаление считается успешным только при
// фактическом удалении: отказ по правам и отсутствие записи — это ошибки.
func TestDeleteMetrics(t *testing.T) {
	success := metrics.AvatarsDeletesTotal.WithLabelValues(metrics.StatusSuccess)
	failed := metrics.AvatarsDeletesTotal.WithLabelValues(metrics.StatusError)

	t.Run("success", func(t *testing.T) {
		before := counterValue(t, success)

		id := uuid.New()
		repo := &stubAvatarRepo{
			getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
				return &domain.Avatar{ID: id, UserID: "user-1"}, nil
			},
		}
		svc := NewAvatarService(repo, &stubObjectStorage{}, &stubEventPublisher{}, nil)
		require.NoError(t, svc.Delete(context.Background(), id, "user-1"))

		assert.Equal(t, before+1, counterValue(t, success))
	})

	t.Run("forbidden", func(t *testing.T) {
		before := counterValue(t, failed)

		id := uuid.New()
		repo := &stubAvatarRepo{
			getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
				return &domain.Avatar{ID: id, UserID: "owner"}, nil
			},
		}
		svc := NewAvatarService(repo, &stubObjectStorage{}, &stubEventPublisher{}, nil)

		err := svc.Delete(context.Background(), id, "intruder")
		require.ErrorIs(t, err, domain.ErrForbidden)

		assert.Equal(t, before+1, counterValue(t, failed))
	})

	t.Run("not found", func(t *testing.T) {
		before := counterValue(t, failed)

		svc := NewAvatarService(&stubAvatarRepo{}, &stubObjectStorage{}, &stubEventPublisher{}, nil)

		err := svc.Delete(context.Background(), uuid.New(), "user-1")
		require.ErrorIs(t, err, domain.ErrNotFound)

		assert.Equal(t, before+1, counterValue(t, failed))
	})
}
