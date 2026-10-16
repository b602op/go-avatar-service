package services

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/b602op/avatars-service/internal/domain"
)

// Compile-time проверки, что стабы реализуют доменные интерфейсы.
var (
	_ domain.AvatarRepository = (*stubAvatarRepo)(nil)
	_ domain.ObjectStorage    = (*stubObjectStorage)(nil)
	_ domain.EventPublisher   = (*stubEventPublisher)(nil)
)

// stubAvatarRepo — ручной стаб репозитория аватарок.
//
// Поведение задаётся через функции-поля; вызовы записываются в поля для
// последующих проверок в тестах.
type stubAvatarRepo struct {
	createFn      func(ctx context.Context, a *domain.Avatar) error
	getByIDFn     func(ctx context.Context, id uuid.UUID) (*domain.Avatar, error)
	listByUserFn  func(ctx context.Context, userID string) ([]*domain.Avatar, error)
	updateProcFn  func(ctx context.Context, id uuid.UUID, status domain.ProcessingStatus, thumbs map[string]string) error
	completeFn    func(ctx context.Context, id uuid.UUID, thumbs map[string]string, width, height int) error
	updateUpFn    func(ctx context.Context, id uuid.UUID, status domain.UploadStatus) error
	softDeleteFn  func(ctx context.Context, id uuid.UUID) error
	listPendingFn func(ctx context.Context, olderThan time.Duration) ([]*domain.Avatar, error)

	created     *domain.Avatar
	softDeleted []uuid.UUID
}

func (s *stubAvatarRepo) Create(ctx context.Context, a *domain.Avatar) error {
	s.created = a
	if s.createFn != nil {
		return s.createFn(ctx, a)
	}
	return nil
}

func (s *stubAvatarRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error) {
	if s.getByIDFn != nil {
		return s.getByIDFn(ctx, id)
	}
	return nil, domain.ErrNotFound
}

func (s *stubAvatarRepo) ListByUserID(ctx context.Context, userID string) ([]*domain.Avatar, error) {
	if s.listByUserFn != nil {
		return s.listByUserFn(ctx, userID)
	}
	return nil, nil
}

func (s *stubAvatarRepo) UpdateProcessingStatus(ctx context.Context, id uuid.UUID, status domain.ProcessingStatus, thumbs map[string]string) error {
	if s.updateProcFn != nil {
		return s.updateProcFn(ctx, id, status, thumbs)
	}
	return nil
}

func (s *stubAvatarRepo) CompleteProcessing(ctx context.Context, id uuid.UUID, thumbs map[string]string, width, height int) error {
	if s.completeFn != nil {
		return s.completeFn(ctx, id, thumbs, width, height)
	}
	return nil
}

func (s *stubAvatarRepo) UpdateUploadStatus(ctx context.Context, id uuid.UUID, status domain.UploadStatus) error {
	if s.updateUpFn != nil {
		return s.updateUpFn(ctx, id, status)
	}
	return nil
}

func (s *stubAvatarRepo) SoftDelete(ctx context.Context, id uuid.UUID) error {
	s.softDeleted = append(s.softDeleted, id)
	if s.softDeleteFn != nil {
		return s.softDeleteFn(ctx, id)
	}
	return nil
}

func (s *stubAvatarRepo) ListPending(ctx context.Context, olderThan time.Duration) ([]*domain.Avatar, error) {
	if s.listPendingFn != nil {
		return s.listPendingFn(ctx, olderThan)
	}
	return nil, nil
}

// stubObjectStorage — ручной стаб объектного хранилища.
type stubObjectStorage struct {
	ensureBucketFn func(ctx context.Context) error
	uploadFn       func(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	downloadFn     func(ctx context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error)
	statFn         func(ctx context.Context, key string) (*domain.ObjectInfo, error)
	deleteFn       func(ctx context.Context, key string) error
	deleteManyFn   func(ctx context.Context, keys []string) error
	healthFn       func(ctx context.Context) error

	uploadedKey  string
	uploadedSize int64
	deletedKeys  []string
}

func (s *stubObjectStorage) EnsureBucket(ctx context.Context) error {
	if s.ensureBucketFn != nil {
		return s.ensureBucketFn(ctx)
	}
	return nil
}

func (s *stubObjectStorage) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	s.uploadedKey = key
	s.uploadedSize = size
	if s.uploadFn != nil {
		return s.uploadFn(ctx, key, r, size, contentType)
	}
	return nil
}

func (s *stubObjectStorage) Download(ctx context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error) {
	if s.downloadFn != nil {
		return s.downloadFn(ctx, key)
	}
	return nil, nil, domain.ErrNotFound
}

func (s *stubObjectStorage) StatObject(ctx context.Context, key string) (*domain.ObjectInfo, error) {
	if s.statFn != nil {
		return s.statFn(ctx, key)
	}
	return nil, domain.ErrNotFound
}

func (s *stubObjectStorage) Delete(ctx context.Context, key string) error {
	if s.deleteFn != nil {
		return s.deleteFn(ctx, key)
	}
	return nil
}

func (s *stubObjectStorage) DeleteMany(ctx context.Context, keys []string) error {
	s.deletedKeys = keys
	if s.deleteManyFn != nil {
		return s.deleteManyFn(ctx, keys)
	}
	return nil
}

func (s *stubObjectStorage) HealthCheck(ctx context.Context) error {
	if s.healthFn != nil {
		return s.healthFn(ctx)
	}
	return nil
}

// stubEventPublisher — ручной стаб паблишера событий.
type stubEventPublisher struct {
	uploadedFn func(ctx context.Context, avatarID uuid.UUID, userID, s3Key string) error
	deletedFn  func(ctx context.Context, avatarID uuid.UUID, userID string, s3Keys []string) error

	uploadedID    uuid.UUID
	uploadedKey   string
	uploadedCount int
	deletedID     uuid.UUID
	deletedKeys   []string
	deletedCount  int
}

func (s *stubEventPublisher) PublishAvatarUploaded(ctx context.Context, avatarID uuid.UUID, userID, s3Key string) error {
	s.uploadedID = avatarID
	s.uploadedKey = s3Key
	s.uploadedCount++
	if s.uploadedFn != nil {
		return s.uploadedFn(ctx, avatarID, userID, s3Key)
	}
	return nil
}

func (s *stubEventPublisher) PublishAvatarDeleted(ctx context.Context, avatarID uuid.UUID, userID string, s3Keys []string) error {
	s.deletedID = avatarID
	s.deletedKeys = s3Keys
	s.deletedCount++
	if s.deletedFn != nil {
		return s.deletedFn(ctx, avatarID, userID, s3Keys)
	}
	return nil
}
