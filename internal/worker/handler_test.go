package worker

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/disintegration/imaging"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/pkg/resizer"
)

// ─── инфраструктура тестов ──────────────────────────────────────────────────

const (
	s3AccessKey = "minioadmin"
	s3SecretKey = "minioadmin"
	testBucket  = "avatars-test"
)

// Параметры поднятой инфраструктуры (заполняются в TestMain).
var (
	infraReady bool
	pgDSN      string
	s3Endpoint string
	rabbitURL  string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests поднимает postgres, silo и rabbitmq, применяет миграцию и запускает тесты.
// Если контейнеры недоступны — тесты скипаются (setupTest), а не падают.
func runTests(m *testing.M) int {
	ctx := context.Background()

	pgContainer, dsn, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres unavailable, worker tests will be skipped: %v\n", err)
		return m.Run()
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	siloContainer, endpoint, err := startSilo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "silo unavailable, worker tests will be skipped: %v\n", err)
		return m.Run()
	}
	defer func() { _ = siloContainer.Terminate(ctx) }()

	rabbitContainer, url, err := startRabbitMQ(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rabbitmq unavailable, worker tests will be skipped: %v\n", err)
		return m.Run()
	}
	defer func() { _ = rabbitContainer.Terminate(ctx) }()

	if err := applyMigration(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "failed to apply migration: %v\n", err)
		return 1
	}

	pgDSN, s3Endpoint, rabbitURL, infraReady = dsn, endpoint, url, true
	return m.Run()
}

// startRabbitMQ поднимает контейнер RabbitMQ и возвращает AMQP-адрес.
func startRabbitMQ(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        "rabbitmq:3-management-alpine",
		ExposedPorts: []string{"5672/tcp"},
		WaitingFor: wait.ForLog("Server startup complete").
			WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", err
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}
	port, err := container.MappedPort(ctx, "5672")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}

	return container, fmt.Sprintf("amqp://guest:guest@%s:%s/", host, port.Port()), nil
}

// startPostgres поднимает контейнер PostgreSQL и возвращает DSN.
func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image: "postgres:16-alpine",
		Env: map[string]string{
			"POSTGRES_USER":     "avatar",
			"POSTGRES_PASSWORD": "avatar",
			"POSTGRES_DB":       "avatars",
		},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", err
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}
	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}

	dsn := fmt.Sprintf("postgres://avatar:avatar@%s:%s/avatars?sslmode=disable", host, port.Port())
	return container, dsn, nil
}

// startSilo поднимает S3-совместимое хранилище и возвращает endpoint.
func startSilo(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.ContainerRequest{
		Image:        "pgsty/silo:latest",
		Cmd:          []string{"server", "/data", "--console-address", ":9001"},
		Env:          map[string]string{"MINIO_ROOT_USER": s3AccessKey, "MINIO_ROOT_PASSWORD": s3SecretKey},
		ExposedPorts: []string{"9000/tcp"},
		WaitingFor: wait.ForHTTP("/minio/health/live").
			WithPort("9000/tcp").
			WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", err
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}
	port, err := container.MappedPort(ctx, "9000")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}

	return container, fmt.Sprintf("%s:%s", host, port.Port()), nil
}

// migrationSQL — схема таблицы avatars (копия migrations/0001 + 0002 без goose).
const migrationSQL = `
CREATE TABLE IF NOT EXISTS avatars (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            VARCHAR(255) NOT NULL,
    file_name          VARCHAR(255) NOT NULL,
    mime_type          VARCHAR(100) NOT NULL,
    size_bytes         BIGINT       NOT NULL,
    s3_key             VARCHAR(500) NOT NULL,
    thumbnail_s3_keys  JSONB,
    upload_status      VARCHAR(50)  NOT NULL DEFAULT 'uploading',
    processing_status  VARCHAR(50)  NOT NULL DEFAULT 'pending',
    width              INTEGER,
    height             INTEGER,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ
)`

// applyMigration создаёт схему в тестовой БД.
func applyMigration(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, migrationSQL)
	return err
}

// ─── окружение теста ────────────────────────────────────────────────────────

// testEnv — реальные зависимости: репозиторий и S3-хранилище.
type testEnv struct {
	repo    *postgres.AvatarRepository
	storage *s3.Client
}

// setupTest чистит таблицу и готовит S3-бакет.
func setupTest(t *testing.T) (*testEnv, context.Context) {
	t.Helper()

	if !infraReady {
		t.Skip("postgres/silo are not available")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, pgDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, "TRUNCATE avatars")
	require.NoError(t, err)

	storage, err := s3.New(s3TestConfig(), nil)
	require.NoError(t, err)
	require.NoError(t, storage.EnsureBucket(ctx))

	return &testEnv{repo: postgres.NewAvatarRepository(pool), storage: storage}, ctx
}

// s3TestConfig собирает конфиг S3 для тестового контейнера.
func s3TestConfig() config.S3Config {
	return config.S3Config{
		Endpoint:  s3Endpoint,
		AccessKey: s3AccessKey,
		SecretKey: s3SecretKey,
		Bucket:    testBucket,
		Region:    "us-east-1",
		UseSSL:    false,
	}
}

// ─── хелперы ────────────────────────────────────────────────────────────────

// mockResizer — ручной стаб Resizer.
type mockResizer struct {
	fn func(data []byte, sizes map[string]resizer.Size) (map[string][]byte, error)
}

func (m *mockResizer) ResizeBatch(data []byte, sizes map[string]resizer.Size) (map[string][]byte, error) {
	if m.fn != nil {
		return m.fn(data, sizes)
	}
	result := make(map[string][]byte, len(sizes))
	for name := range sizes {
		result[name] = []byte("fake-jpeg-" + name)
	}
	return result, nil
}

// testLogger возвращает логгер, пишущий в никуда (тесты не засоряют вывод).
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeJPEG генерирует тестовое изображение заданного размера.
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}

	var buf bytes.Buffer
	require.NoError(t, imaging.Encode(&buf, img, imaging.JPEG))
	return buf.Bytes()
}

// newPendingAvatar создаёт в БД запись со статусом pending и уникальным ключом.
//
//nolint:revive // context-as-argument: в тестовых хелперах первым идёт *testing.T
func newPendingAvatar(t *testing.T, env *testEnv, ctx context.Context) *domain.Avatar {
	t.Helper()

	avatarID := uuid.New()
	userID := "user-" + uuid.NewString()[:8]
	now := time.Now().UTC().Truncate(time.Microsecond)

	a := &domain.Avatar{
		ID:               avatarID,
		UserID:           userID,
		FileName:         "photo.jpg",
		MimeType:         "image/jpeg",
		SizeBytes:        1024,
		S3Key:            services.OriginalKey(userID, avatarID, "photo.jpg"),
		UploadStatus:     domain.UploadStatusUploaded,
		ProcessingStatus: domain.ProcessingStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, env.repo.Create(ctx, a))
	return a
}

// uploadOriginal кладёт оригинал в S3 по ключу аватара.
//
//nolint:revive // context-as-argument: в тестовых хелперах первым идёт *testing.T
func uploadOriginal(t *testing.T, env *testEnv, ctx context.Context, a *domain.Avatar, data []byte) {
	t.Helper()
	require.NoError(t, env.storage.Upload(ctx, a.S3Key, bytes.NewReader(data), int64(len(data)), "image/jpeg"))
}

// uploadEvent собирает событие загрузки для аватара.
func uploadEvent(a *domain.Avatar) publisher.AvatarUploadEvent {
	return publisher.AvatarUploadEvent{
		AvatarID:  a.ID.String(),
		UserID:    a.UserID,
		S3Key:     a.S3Key,
		Timestamp: time.Now().Unix(),
	}
}

// ─── HandleUpload ───────────────────────────────────────────────────────────

func TestHandler_HandleUpload_Success(t *testing.T) {
	env, ctx := setupTest(t)
	a := newPendingAvatar(t, env, ctx)
	uploadOriginal(t, env, ctx, a, makeJPEG(t, 500, 500))

	// Реальный ресайз: проверяем всю цепочку, включая фактические размеры.
	h := NewHandler(env.repo, env.storage, ResizerFunc(resizer.ResizeBatch), nil)
	require.NoError(t, h.HandleUpload(ctx, uploadEvent(a)))

	got, err := env.repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ProcessingStatusCompleted, got.ProcessingStatus)
	require.Len(t, got.ThumbnailS3Keys, len(resizer.DefaultSizes))

	// Размеры оригинала сохранены (заполняются через CompleteProcessing).
	require.NotNil(t, got.Width, "width не заполнен")
	require.NotNil(t, got.Height, "height не заполнен")
	assert.Equal(t, 500, *got.Width)
	assert.Equal(t, 500, *got.Height)

	for size, key := range got.ThumbnailS3Keys {
		rc, _, err := env.storage.Download(ctx, key)
		require.NoError(t, err)

		data, err := io.ReadAll(rc)
		_ = rc.Close()
		require.NoError(t, err)

		img, err := imaging.Decode(bytes.NewReader(data))
		require.NoError(t, err)

		want := resizer.DefaultSizes[size]
		assert.LessOrEqual(t, img.Bounds().Dx(), want.Width, "миниатюра %q шире рамки", size)
		assert.LessOrEqual(t, img.Bounds().Dy(), want.Height, "миниатюра %q выше рамки", size)
	}
}

func TestHandler_HandleUpload_NotFound(t *testing.T) {
	env, ctx := setupTest(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)

	// Аватар не существует → мягкое завершение (ack), без ошибки.
	err := h.HandleUpload(ctx, publisher.AvatarUploadEvent{
		AvatarID:  uuid.NewString(),
		UserID:    "user-1",
		S3Key:     "originals/user-1/missing/photo.jpg",
		Timestamp: time.Now().Unix(),
	})
	assert.NoError(t, err)
}

func TestHandler_HandleUpload_AlreadyCompleted(t *testing.T) {
	env, ctx := setupTest(t)
	a := newPendingAvatar(t, env, ctx)

	require.NoError(t, env.repo.UpdateProcessingStatus(ctx, a.ID, domain.ProcessingStatusCompleted, map[string]string{
		"100x100": services.ThumbnailKey(a.UserID, a.ID, "100x100"),
	}))

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)
	require.NoError(t, h.HandleUpload(ctx, uploadEvent(a)))

	// Повторной обработки не было: миниатюра 300x300 в S3 не появилась.
	_, err := env.storage.StatObject(ctx, services.ThumbnailKey(a.UserID, a.ID, "300x300"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestHandler_HandleUpload_InvalidID(t *testing.T) {
	env, ctx := setupTest(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)

	err := h.HandleUpload(ctx, publisher.AvatarUploadEvent{
		AvatarID:  "not-a-uuid",
		UserID:    "user-1",
		S3Key:     "originals/user-1/x/photo.jpg",
		Timestamp: time.Now().Unix(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker: parse avatar_id")
}

func TestHandler_HandleUpload_S3DownloadFails(t *testing.T) {
	env, ctx := setupTest(t)

	// Запись есть, но объекта в S3 нет.
	a := newPendingAvatar(t, env, ctx)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)

	err := h.HandleUpload(ctx, uploadEvent(a))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker: download original")
}

func TestHandler_HandleUpload_ResizeFails(t *testing.T) {
	env, ctx := setupTest(t)
	a := newPendingAvatar(t, env, ctx)
	uploadOriginal(t, env, ctx, a, makeJPEG(t, 200, 200))

	rz := &mockResizer{fn: func([]byte, map[string]resizer.Size) (map[string][]byte, error) {
		return nil, assert.AnError
	}}
	h := NewHandler(env.repo, env.storage, rz, nil)

	err := h.HandleUpload(ctx, uploadEvent(a))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker: resize")

	// Статус не переведён в completed — событие можно повторить.
	got, err := env.repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ProcessingStatusPending, got.ProcessingStatus)
}

func TestHandler_HandleUpload_InvalidImage(t *testing.T) {
	env, ctx := setupTest(t)
	a := newPendingAvatar(t, env, ctx)

	// В S3 лежит не изображение: GetDimensions должен упасть до ресайза.
	uploadOriginal(t, env, ctx, a, []byte("definitely not an image"))

	// Ресайзер настоящий: ошибка обязана возникнуть на шаге размеров, а не ресайза.
	h := NewHandler(env.repo, env.storage, ResizerFunc(resizer.ResizeBatch), nil)

	err := h.HandleUpload(ctx, uploadEvent(a))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker: get dimensions")

	// Статус не переведён в completed — событие можно повторить.
	got, err := env.repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ProcessingStatusPending, got.ProcessingStatus)
	assert.Nil(t, got.Width)
	assert.Nil(t, got.Height)
}

// ─── HandleDelete ───────────────────────────────────────────────────────────

func TestHandler_HandleDelete_Success(t *testing.T) {
	env, ctx := setupTest(t)

	keys := []string{
		"originals/user-1/" + uuid.NewString() + "/photo.jpg",
		"thumbnails/user-1/" + uuid.NewString() + "/100x100.jpg",
		"thumbnails/user-1/" + uuid.NewString() + "/300x300.jpg",
	}
	for _, k := range keys {
		data := []byte(k)
		require.NoError(t, env.storage.Upload(ctx, k, bytes.NewReader(data), int64(len(data)), "image/jpeg"))
	}

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)
	require.NoError(t, h.HandleDelete(ctx, publisher.AvatarDeleteEvent{
		AvatarID:  uuid.NewString(),
		UserID:    "user-1",
		S3Keys:    keys,
		Timestamp: time.Now().Unix(),
	}))

	for _, k := range keys {
		_, err := env.storage.StatObject(ctx, k)
		assert.ErrorIs(t, err, domain.ErrNotFound, "объект %q не удалён", k)
	}
}

func TestHandler_HandleDelete_EmptyKeys(t *testing.T) {
	env, ctx := setupTest(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)
	err := h.HandleDelete(ctx, publisher.AvatarDeleteEvent{
		AvatarID:  uuid.NewString(),
		UserID:    "user-1",
		S3Keys:    nil,
		Timestamp: time.Now().Unix(),
	})
	assert.NoError(t, err)
}

func TestHandler_HandleDelete_InvalidID(t *testing.T) {
	env, ctx := setupTest(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)
	err := h.HandleDelete(ctx, publisher.AvatarDeleteEvent{
		AvatarID:  "not-a-uuid",
		UserID:    "user-1",
		S3Keys:    []string{"some/key"},
		Timestamp: time.Now().Unix(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker: parse avatar_id")
}

func TestHandler_HandleDelete_NotExists(t *testing.T) {
	env, ctx := setupTest(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, nil)

	// Ключей в S3 нет — удаление идемпотентно, ошибки быть не должно.
	err := h.HandleDelete(ctx, publisher.AvatarDeleteEvent{
		AvatarID:  uuid.NewString(),
		UserID:    "user-1",
		S3Keys:    []string{"originals/user-1/missing/photo.jpg"},
		Timestamp: time.Now().Unix(),
	})
	assert.NoError(t, err)
}
