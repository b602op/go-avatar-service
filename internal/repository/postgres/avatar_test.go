package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
)

// testDSN собирается из env или дефолта для локальной БД avatars_test.
func testDSN() string {
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://avatar:avatar@localhost:5432/avatars_test?sslmode=disable"
}

// setupTest создаёт пул, чистит таблицу и возвращает репозиторий.
func setupTest(t *testing.T) (*AvatarRepository, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN())
	require.NoError(t, err, "не удалось подключиться к тестовой БД (запусти 'make up' и создай avatars_test)")

	_, err = pool.Exec(ctx, "TRUNCATE avatars")
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
	})

	return NewAvatarRepository(pool), pool
}

// newAvatar — хелпер для создания тестового аватара.
func newAvatar(userID string) *domain.Avatar {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &domain.Avatar{
		ID:               uuid.New(),
		UserID:           userID,
		FileName:         "photo.jpg",
		MimeType:         "image/jpeg",
		SizeBytes:        1024000,
		S3Key:            "originals/" + uuid.NewString() + ".jpg",
		UploadStatus:     domain.UploadStatusUploaded,
		ProcessingStatus: domain.ProcessingStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func TestAvatarRepository_CreateAndGet(t *testing.T) {
	repo, _ := setupTest(t)
	ctx := context.Background()

	a := newAvatar("user-1")
	require.NoError(t, repo.Create(ctx, a))

	got, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, "user-1", got.UserID)
	assert.Equal(t, "photo.jpg", got.FileName)
	assert.Equal(t, domain.UploadStatusUploaded, got.UploadStatus)
	assert.Equal(t, domain.ProcessingStatusPending, got.ProcessingStatus)
	assert.Nil(t, got.DeletedAt)
}

func TestAvatarRepository_GetByID_NotFound(t *testing.T) {
	repo, _ := setupTest(t)

	_, err := repo.GetByID(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAvatarRepository_ListByUserID(t *testing.T) {
	repo, _ := setupTest(t)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, newAvatar("user-1")))
	require.NoError(t, repo.Create(ctx, newAvatar("user-1")))
	require.NoError(t, repo.Create(ctx, newAvatar("user-2")))

	list, err := repo.ListByUserID(ctx, "user-1")
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestAvatarRepository_UpdateProcessingStatus(t *testing.T) {
	repo, _ := setupTest(t)
	ctx := context.Background()

	a := newAvatar("user-1")
	require.NoError(t, repo.Create(ctx, a))

	thumbs := map[string]string{
		"100x100": "thumbnails/" + a.ID.String() + "/100x100.jpg",
		"300x300": "thumbnails/" + a.ID.String() + "/300x300.jpg",
	}
	require.NoError(t, repo.UpdateProcessingStatus(ctx, a.ID, domain.ProcessingStatusCompleted, thumbs))

	got, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ProcessingStatusCompleted, got.ProcessingStatus)
	assert.Equal(t, thumbs, got.ThumbnailS3Keys)
}

func TestAvatarRepository_UpdateProcessingStatus_NotFound(t *testing.T) {
	repo, _ := setupTest(t)

	err := repo.UpdateProcessingStatus(context.Background(), uuid.New(), domain.ProcessingStatusCompleted, nil)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAvatarRepository_SoftDelete(t *testing.T) {
	repo, _ := setupTest(t)
	ctx := context.Background()

	a := newAvatar("user-1")
	require.NoError(t, repo.Create(ctx, a))
	require.NoError(t, repo.SoftDelete(ctx, a.ID))

	_, err := repo.GetByID(ctx, a.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	list, err := repo.ListByUserID(ctx, "user-1")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestAvatarRepository_SoftDelete_NotFound(t *testing.T) {
	repo, _ := setupTest(t)

	err := repo.SoftDelete(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAvatarRepository_ListPending(t *testing.T) {
	repo, pool := setupTest(t)
	ctx := context.Background()

	// Свежий pending — не должен попасть.
	fresh := newAvatar("user-1")
	require.NoError(t, repo.Create(ctx, fresh))

	// Старый pending — должен попасть. Сдвигаем created_at вручную.
	old := newAvatar("user-1")
	require.NoError(t, repo.Create(ctx, old))
	_, err := pool.Exec(ctx, "UPDATE avatars SET created_at = NOW() - INTERVAL '10 minutes' WHERE id = $1", old.ID)
	require.NoError(t, err)

	pending, err := repo.ListPending(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, old.ID, pending[0].ID)
}
