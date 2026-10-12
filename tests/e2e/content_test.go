// Package e2e содержит сквозные тесты HTTP-слоя: запрос идёт через реальный
// роутер, сервис, PostgreSQL и S3-совместимое хранилище.
//
// Отличие от юнит-тестов хендлеров (со стабами) — здесь проверяется вся цепочка
// целиком: маршрут, middleware, валидация, БД, хранилище и заголовки ответа.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	"github.com/b602op/avatars-service/internal/api"
	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/handlers"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/internal/worker"
	"github.com/b602op/avatars-service/pkg/resizer"
)

// ─── инфраструктура ─────────────────────────────────────────────────────────

const (
	s3AccessKey = "minioadmin"
	s3SecretKey = "minioadmin"
	testBucket  = "avatars-e2e"
	userID      = "e2e-user"
)

// Параметры поднятой инфраструктуры (заполняются в TestMain).
var (
	infraReady bool
	pgDSN      string
	s3Endpoint string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests поднимает postgres и S3, применяет миграцию и запускает тесты.
// Если контейнеры недоступны — тесты скипаются (newEnv), а не падают.
func runTests(m *testing.M) int {
	ctx := context.Background()

	pgContainer, dsn, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres unavailable, e2e tests will be skipped: %v\n", err)
		return m.Run()
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	siloContainer, endpoint, err := startSilo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "silo unavailable, e2e tests will be skipped: %v\n", err)
		return m.Run()
	}
	defer func() { _ = siloContainer.Terminate(ctx) }()

	if err := applyMigration(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "failed to apply migration: %v\n", err)
		return 1
	}

	pgDSN, s3Endpoint, infraReady = dsn, endpoint, true
	return m.Run()
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

// migrationSQL — схема таблицы avatars (копия migrations/0001_init.sql без goose).
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

// stubPublisher — заглушка брокера: эндпоинту выдачи содержимого RabbitMQ не нужен.
type stubPublisher struct{}

func (stubPublisher) PublishAvatarUploaded(context.Context, uuid.UUID, string, string) error {
	return nil
}

func (stubPublisher) PublishAvatarDeleted(context.Context, uuid.UUID, string, []string) error {
	return nil
}

// Проверка на этапе компиляции, что заглушка реализует доменный интерфейс.
var _ domain.EventPublisher = stubPublisher{}

// env — поднятое приложение: реальный роутер поверх реальных БД и S3.
type env struct {
	baseURL string
	client  *http.Client
	repo    *postgres.AvatarRepository
	storage *s3.Client
}

// newEnv чистит таблицу, готовит бакет и поднимает HTTP-сервер приложения.
func newEnv(t *testing.T) *env {
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

	storage, err := s3.New(s3Config(), testLogger())
	require.NoError(t, err)
	require.NoError(t, storage.EnsureBucket(ctx))

	repo := postgres.NewAvatarRepository(pool)
	svc := services.NewAvatarService(repo, storage, stubPublisher{}, testLogger())
	router := api.NewRouter(api.RouterDeps{
		Log:            testLogger(),
		AvatarHandler:  handlers.NewAvatarHandler(svc, testLogger()),
		AllowedOrigins: []string{"*"},
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &env{baseURL: server.URL, client: server.Client(), repo: repo, storage: storage}
}

// s3Config собирает конфиг S3 для тестового контейнера.
func s3Config() config.S3Config {
	return config.S3Config{
		Endpoint:  s3Endpoint,
		AccessKey: s3AccessKey,
		SecretKey: s3SecretKey,
		Bucket:    testBucket,
		Region:    "us-east-1",
		UseSSL:    false,
	}
}

// testLogger возвращает логгер, пишущий в никуда.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// upload загружает оригинал через POST /api/v1/avatars и возвращает его ID.
func (e *env) upload(t *testing.T, data []byte) string {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "photo.jpg")
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/api/v1/avatars", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-User-ID", userID)

	resp, err := e.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var out handlers.UploadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.ID)
	return out.ID
}

// process имитирует шаг обработки: ровно та же логика, что у воркера в проде
// (скачивание оригинала, ресайз, загрузка миниатюр, перевод в completed).
func (e *env) process(t *testing.T, avatarID string) {
	t.Helper()

	ctx := context.Background()
	id, err := uuid.Parse(avatarID)
	require.NoError(t, err)

	avatar, err := e.repo.GetByID(ctx, id)
	require.NoError(t, err)

	h := worker.NewHandler(e.repo, e.storage, worker.ResizerFunc(resizer.ResizeBatch), testLogger())
	require.NoError(t, h.HandleUpload(ctx, publisher.AvatarUploadEvent{
		AvatarID: avatarID,
		UserID:   avatar.UserID,
		S3Key:    avatar.S3Key,
	}))
}

// getContent запрашивает содержимое аватарки и возвращает ответ и его тело.
func (e *env) getContent(t *testing.T, avatarID, size string) (*http.Response, []byte) {
	t.Helper()

	target := e.baseURL + "/api/v1/avatars/" + avatarID + "/content"
	if size != "" {
		target += "?size=" + url.QueryEscape(size)
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	req.Header.Set("X-User-ID", userID)

	resp, err := e.client.Do(req)
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	return resp, body
}

// testJPEG генерирует JPEG заданного размера с узором из точек.
//
// Сплошная заливка сжалась бы почти в ноль, поэтому добавляем «шум»: так
// проверка байтов и декодирования остаётся осмысленной.
func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := imaging.New(width, height, color.NRGBA{R: 20, G: 40, B: 200, A: 255})
	for x := 0; x < width; x += 8 {
		for y := 0; y < height; y += 8 {
			img.Set(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}

	var buf bytes.Buffer
	require.NoError(t, imaging.Encode(&buf, img, imaging.JPEG))
	return buf.Bytes()
}

// ─── тесты ──────────────────────────────────────────────────────────────────

// TestE2E_GetContent_ReturnsBytes проверяет выдачу оригинала и миниатюры.
func TestE2E_GetContent_ReturnsBytes(t *testing.T) {
	e := newEnv(t)

	original := testJPEG(t, 640, 480)
	avatarID := e.upload(t, original)
	e.process(t, avatarID)

	// Миниатюра: пропорции 640x480 → 100x75, формат всегда JPEG.
	resp, body := e.getContent(t, avatarID, "100x100")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	assert.Equal(t, fmt.Sprintf("%d", len(body)), resp.Header.Get("Content-Length"))
	assert.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"))
	assert.NotEmpty(t, resp.Header.Get("ETag"))

	decoded, err := imaging.Decode(bytes.NewReader(body))
	require.NoError(t, err, "тело ответа должно быть валидным JPEG")
	assert.Equal(t, 100, decoded.Bounds().Dx())
	assert.Equal(t, 75, decoded.Bounds().Dy())

	// Оригинал: байты совпадают с загруженными.
	resp, body = e.getContent(t, avatarID, "original")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, original, body)
}

// TestE2E_GetContent_UnknownSize проверяет 501 и подсказку по доступным размерам.
func TestE2E_GetContent_UnknownSize(t *testing.T) {
	e := newEnv(t)

	avatarID := e.upload(t, testJPEG(t, 200, 200))

	resp, body := e.getContent(t, avatarID, "500x500")
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	assert.JSONEq(t,
		`{"error":"Not Implemented","details":"available sizes: original, 100x100, 300x300"}`,
		string(body))
}
