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

func TestAvatarService_GetContent_Original_Success(t *testing.T) {
	avatar := &domain.Avatar{
		ID:     uuid.New(),
		UserID: "user-1",
		S3Key:  "originals/user-1/x/photo.jpg",
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
			return body, &domain.ObjectInfo{Key: key, Size: 7}, nil
		},
	}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	rc, info, err := svc.GetContent(context.Background(), avatar.ID, sizeOriginal)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	assert.Equal(t, int64(7), info.Size)
}

func TestAvatarService_GetContent_Resize_NotImplemented(t *testing.T) {
	avatar := &domain.Avatar{ID: uuid.New(), UserID: "user-1", S3Key: "originals/user-1/x/photo.jpg"}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	svc := NewAvatarService(repo, &stubObjectStorage{}, &stubEventPublisher{}, nil)

	_, _, err := svc.GetContent(context.Background(), avatar.ID, "100x100")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
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

	// Удалены оригинал и обе миниатюры.
	assert.ElementsMatch(t, []string{
		"originals/user-1/a1/photo.jpg",
		"thumbnails/user-1/a1/100x100.jpg",
		"thumbnails/user-1/a1/300x300.jpg",
	}, storage.deletedKeys)

	// Событие об удалении опубликовано.
	assert.Equal(t, 1, pub.deletedCount)
	assert.Equal(t, avatar.ID, pub.deletedID)
	assert.ElementsMatch(t, storage.deletedKeys, pub.deletedKeys)
}

func TestAvatarService_Delete_Forbidden(t *testing.T) {
	avatar := &domain.Avatar{ID: uuid.New(), UserID: "user-1", S3Key: "originals/user-1/a1/photo.jpg"}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	err := svc.Delete(context.Background(), avatar.ID, "user-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrForbidden)

	// Чужой аватар не удаляется: ни в БД, ни в S3.
	assert.Empty(t, repo.softDeleted)
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

func TestAvatarService_Delete_StorageDeleteFails(t *testing.T) {
	storageErr := errors.New("s3 unavailable")
	avatar := &domain.Avatar{ID: uuid.New(), UserID: "user-1", S3Key: "originals/user-1/a1/photo.jpg"}
	repo := &stubAvatarRepo{
		getByIDFn: func(context.Context, uuid.UUID) (*domain.Avatar, error) {
			return avatar, nil
		},
	}
	storage := &stubObjectStorage{
		deleteManyFn: func(context.Context, []string) error {
			return storageErr
		},
	}
	svc := NewAvatarService(repo, storage, &stubEventPublisher{}, nil)

	err := svc.Delete(context.Background(), avatar.ID, "user-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, storageErr)

	// Soft-delete в БД уже сделан: запись помечена удалённой до сбоя очистки S3.
	assert.Contains(t, repo.softDeleted, avatar.ID)
}
