package services

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
)

func TestAvatarService_Upload_Success(t *testing.T) {
	repo := &stubAvatarRepo{}
	storage := &stubObjectStorage{}
	pub := &stubEventPublisher{}
	svc := NewAvatarService(repo, storage, pub, nil)

	got, err := svc.Upload(context.Background(), "user-1", "photo.jpg", mimeJPEG, jpegData)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Репозиторий получил Create с корректной моделью.
	require.NotNil(t, repo.created)
	assert.Equal(t, "user-1", repo.created.UserID)
	assert.Equal(t, "photo.jpg", repo.created.FileName)
	assert.Equal(t, mimeJPEG, repo.created.MimeType)
	assert.Equal(t, int64(len(jpegData)), repo.created.SizeBytes)
	assert.Equal(t, domain.UploadStatusUploaded, repo.created.UploadStatus)
	assert.Equal(t, domain.ProcessingStatusPending, repo.created.ProcessingStatus)
	assert.Equal(t, got.ID, repo.created.ID)

	// Ключ в S3 сгенерирован по правилу originals/{userID}/{avatarID}/{fileName}.
	wantKey := OriginalKey("user-1", got.ID, "photo.jpg")
	assert.Equal(t, wantKey, got.S3Key)
	assert.Equal(t, wantKey, storage.uploadedKey)
	assert.Equal(t, int64(len(jpegData)), storage.uploadedSize)

	// Событие опубликовано.
	assert.Equal(t, 1, pub.uploadedCount)
	assert.Equal(t, got.ID, pub.uploadedID)
	assert.Equal(t, wantKey, pub.uploadedKey)
}

func TestAvatarService_Upload_InvalidFormat(t *testing.T) {
	svc := NewAvatarService(&stubAvatarRepo{}, &stubObjectStorage{}, &stubEventPublisher{}, nil)

	_, err := svc.Upload(context.Background(), "user-1", "bad.txt", "text/plain", []byte("hello"))
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}

func TestAvatarService_Upload_FileTooLarge(t *testing.T) {
	data := make([]byte, MaxFileSize+1)
	copy(data, jpegData)

	svc := NewAvatarService(&stubAvatarRepo{}, &stubObjectStorage{}, &stubEventPublisher{}, nil)

	_, err := svc.Upload(context.Background(), "user-1", "big.jpg", mimeJPEG, data)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrFileTooLarge)
}

func TestAvatarService_Upload_StorageFails(t *testing.T) {
	storageErr := errors.New("s3 unavailable")
	repo := &stubAvatarRepo{}
	storage := &stubObjectStorage{
		uploadFn: func(context.Context, string, io.Reader, int64, string) error {
			return storageErr
		},
	}
	pub := &stubEventPublisher{}
	svc := NewAvatarService(repo, storage, pub, nil)

	_, err := svc.Upload(context.Background(), "user-1", "photo.jpg", mimeJPEG, jpegData)
	require.Error(t, err)
	assert.ErrorIs(t, err, storageErr)

	// При сбое S3 запись в БД не создаётся и событие не публикуется.
	assert.Nil(t, repo.created)
	assert.Zero(t, pub.uploadedCount)
}

func TestAvatarService_GetContent_Original(t *testing.T) {
	// Пустой size и "original" — одно и то же: отдаём оригинал.
	for _, size := range []string{"", sizeOriginal} {
		t.Run("size="+size, func(t *testing.T) {
			avatar := &domain.Avatar{
				ID:       uuid.New(),
				UserID:   "user-1",
				MimeType: mimeJPEG,
				S3Key:    "originals/user-1/x/photo.jpg",
				ThumbnailS3Keys: map[string]string{
					"100x100": "thumbnails/user-1/x/100x100.jpg",
				},
			}
			repo := &stubAvatarRepo{
				getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
					return avatar, nil
				},
			}
			body := io.NopCloser(strings.NewReader("content"))
			storage := &stubObjectStorage{
				downloadFn: func(_ context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error) {
					assert.Equal(t, avatar.S3Key, key)
					// ContentType пустой — сервис обязан подставить его сам.
					return body, &domain.ObjectInfo{Key: key, Size: 7}, nil
				},
			}
			svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

			rc, info, err := svc.GetContent(context.Background(), avatar.ID, size)
			require.NoError(t, err)
			defer func() { _ = rc.Close() }()

			assert.Equal(t, int64(7), info.Size)
			assert.Equal(t, mimeJPEG, info.ContentType)
		})
	}
}

func TestAvatarService_GetContent_KeepsStorageContentType(t *testing.T) {
	avatar := &domain.Avatar{
		ID:       uuid.New(),
		UserID:   "user-1",
		MimeType: mimeJPEG,
		S3Key:    "originals/user-1/x/photo.jpg",
	}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{
		downloadFn: func(_ context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error) {
			return io.NopCloser(strings.NewReader("content")),
				&domain.ObjectInfo{Key: key, Size: 7, ContentType: "image/png"}, nil
		},
	}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	rc, info, err := svc.GetContent(context.Background(), avatar.ID, sizeOriginal)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	// MIME из хранилища приоритетнее: подстановка нужна только как fallback.
	assert.Equal(t, "image/png", info.ContentType)
}

func TestAvatarService_GetContent_Thumbnail(t *testing.T) {
	avatar := &domain.Avatar{
		ID:       uuid.New(),
		UserID:   "user-1",
		MimeType: mimeJPEG,
		S3Key:    "originals/user-1/x/photo.jpg",
		ThumbnailS3Keys: map[string]string{
			"100x100": "thumbnails/user-1/x/100x100.jpg",
			"300x300": "thumbnails/user-1/x/300x300.jpg",
		},
	}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	body := io.NopCloser(strings.NewReader("thumb"))
	storage := &stubObjectStorage{
		downloadFn: func(_ context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error) {
			assert.Equal(t, "thumbnails/user-1/x/100x100.jpg", key)
			return body, &domain.ObjectInfo{Key: key, Size: 5}, nil
		},
	}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	rc, info, err := svc.GetContent(context.Background(), avatar.ID, "100x100")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	assert.Equal(t, int64(5), info.Size)
	// Миниатюры всегда JPEG, даже если mime_type оригинала другой.
	assert.Equal(t, "image/jpeg", info.ContentType)
}

func TestAvatarService_GetContent_UnknownSize(t *testing.T) {
	avatar := &domain.Avatar{
		ID:     uuid.New(),
		UserID: "user-1",
		S3Key:  "originals/user-1/x/photo.jpg",
		ThumbnailS3Keys: map[string]string{
			"100x100": "thumbnails/user-1/x/100x100.jpg",
		},
	}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{
		downloadFn: func(context.Context, string) (io.ReadCloser, *domain.ObjectInfo, error) {
			t.Fatal("download не должен вызываться для неизвестного размера")
			return nil, nil, nil
		},
	}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	_, _, err := svc.GetContent(context.Background(), avatar.ID, "500x500")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
}

func TestAvatarService_GetContent_NotFound(t *testing.T) {
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return nil, domain.ErrNotFound
		},
	}
	svc := NewAvatarService(repo, &stubObjectStorage{}, &stubEventPublisher{}, nil)

	_, _, err := svc.GetContent(context.Background(), uuid.New(), sizeOriginal)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAvatarService_Delete_Success(t *testing.T) {
	avatar := &domain.Avatar{
		ID:     uuid.New(),
		UserID: "user-1",
		S3Key:  "originals/user-1/a1/photo.jpg",
		ThumbnailS3Keys: map[string]string{
			"100x100": "thumbnails/user-1/a1/100x100.jpg",
			"300x300": "thumbnails/user-1/a1/300x300.jpg",
		},
	}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{}
	pub := &stubEventPublisher{}
	svc := NewAvatarService(repo, storage, pub, nil)

	err := svc.Delete(context.Background(), avatar.ID, "user-1")
	require.NoError(t, err)

	// Soft-delete выполнен.
	assert.Contains(t, repo.softDeleted, avatar.ID)

	// S3 в Delete не трогаем: очистка асинхронная, через воркер.
	assert.Empty(t, storage.deletedKeys)

	// Событие об удалении опубликовано с ключами оригинала и миниатюр.
	assert.Equal(t, 1, pub.deletedCount)
	assert.Equal(t, avatar.ID, pub.deletedID)
	assert.ElementsMatch(t, []string{
		"originals/user-1/a1/photo.jpg",
		"thumbnails/user-1/a1/100x100.jpg",
		"thumbnails/user-1/a1/300x300.jpg",
	}, pub.deletedKeys)
}

func TestAvatarService_Delete_Forbidden(t *testing.T) {
	avatar := &domain.Avatar{ID: uuid.New(), UserID: "user-1", S3Key: "originals/user-1/a1/photo.jpg"}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{}
	pub := &stubEventPublisher{}
	svc := NewAvatarService(repo, storage, pub, nil)

	err := svc.Delete(context.Background(), avatar.ID, "user-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrForbidden)

	// Чужой аватар не удаляется: ни в БД, ни событие не публикуется.
	assert.Empty(t, repo.softDeleted)
	assert.Zero(t, pub.deletedCount)
	assert.Empty(t, storage.deletedKeys)
}

func TestAvatarService_Delete_NotFound(t *testing.T) {
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return nil, domain.ErrNotFound
		},
	}
	svc := NewAvatarService(repo, &stubObjectStorage{}, &stubEventPublisher{}, nil)

	err := svc.Delete(context.Background(), uuid.New(), "user-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Empty(t, repo.softDeleted)
}

func TestAvatarService_Delete_PublishFails(t *testing.T) {
	pubErr := errors.New("rabbitmq unavailable")
	avatar := &domain.Avatar{ID: uuid.New(), UserID: "user-1", S3Key: "originals/user-1/a1/photo.jpg"}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{}
	pub := &stubEventPublisher{
		deletedFn: func(context.Context, uuid.UUID, string, []string) error {
			return pubErr
		},
	}
	svc := NewAvatarService(repo, storage, pub, nil)

	err := svc.Delete(context.Background(), avatar.ID, "user-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, pubErr)

	// Soft-delete уже сделан; S3 не трогаем — запись останется «зависшей»
	// и будет подобрана cleanup-воркером (outbox-lite).
	assert.Contains(t, repo.softDeleted, avatar.ID)
	assert.Empty(t, storage.deletedKeys)
}
