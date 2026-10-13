package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
)

// ─── стабы ──────────────────────────────────────────────────────────────────

// stubDB — стаб проверки БД.
type stubDB struct {
	pingErr error
	// blockUntilCtxDone имитирует зависший пул: Ping ждёт отмены контекста.
	blockUntilCtxDone bool
}

func (s *stubDB) Ping(ctx context.Context) error {
	if s.blockUntilCtxDone {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.pingErr
}

// stubStorage — стаб объектного хранилища. Значим только HealthCheck,
// остальные методы — заглушки для реализации domain.ObjectStorage.
type stubStorage struct {
	healthErr error
}

func (s *stubStorage) HealthCheck(context.Context) error { return s.healthErr }

func (s *stubStorage) EnsureBucket(context.Context) error { return nil }

func (s *stubStorage) Upload(context.Context, string, io.Reader, int64, string) error { return nil }

func (s *stubStorage) Download(context.Context, string) (io.ReadCloser, *domain.ObjectInfo, error) {
	return nil, nil, domain.ErrNotFound
}

func (s *stubStorage) StatObject(context.Context, string) (*domain.ObjectInfo, error) {
	return nil, domain.ErrNotFound
}

func (s *stubStorage) Delete(context.Context, string) error { return nil }

func (s *stubStorage) DeleteMany(context.Context, []string) error { return nil }

// stubBroker — стаб брокера.
type stubBroker struct {
	closed bool
}

func (s *stubBroker) IsClosed() bool { return s.closed }

// Проверка на этапе компиляции, что стабы реализуют интерфейсы.
var (
	_ DBHealthChecker      = (*stubDB)(nil)
	_ domain.ObjectStorage = (*stubStorage)(nil)
	_ HealthChecker        = (*stubBroker)(nil)
)

// ─── helpers ────────────────────────────────────────────────────────────────

// healthLogger возвращает логгер, пишущий в никуда.
func healthLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// doHealth выполняет GET /health против собранного хендлера.
func doHealth(t *testing.T, h http.HandlerFunc) (*httptest.ResponseRecorder, HealthResponse) {
	t.Helper()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return rec, resp
}

// ─── тесты ──────────────────────────────────────────────────────────────────

func TestHealth_AllOK(t *testing.T) {
	h := Health(&stubDB{}, &stubStorage{}, &stubBroker{}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))
	assert.Equal(t, statusOK, resp.Status)
	require.Len(t, resp.Components, 3)
	for name, comp := range resp.Components {
		assert.Equal(t, componentOK, comp.Status, "компонент %q должен быть ok", name)
		assert.Empty(t, comp.Error, "у успешного компонента не должно быть error")
	}
}

func TestHealth_DBFail(t *testing.T) {
	h := Health(&stubDB{pingErr: errors.New("dial tcp: connection refused")},
		&stubStorage{}, &stubBroker{}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDegraded, resp.Status)
	assert.Equal(t, componentFail, resp.Components[componentDB].Status)
	assert.Equal(t, "dial tcp: connection refused", resp.Components[componentDB].Error)
	// Остальные компоненты проверены независимо и остались живы.
	assert.Equal(t, componentOK, resp.Components[componentS3].Status)
	assert.Equal(t, componentOK, resp.Components[componentBroker].Status)
}

func TestHealth_S3Fail(t *testing.T) {
	h := Health(&stubDB{}, &stubStorage{healthErr: errors.New("healthcheck: bucket not found")},
		&stubBroker{}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDegraded, resp.Status)
	assert.Equal(t, componentFail, resp.Components[componentS3].Status)
	assert.Equal(t, "healthcheck: bucket not found", resp.Components[componentS3].Error)
	assert.Equal(t, componentOK, resp.Components[componentDB].Status)
}

func TestHealth_BrokerClosed(t *testing.T) {
	h := Health(&stubDB{}, &stubStorage{}, &stubBroker{closed: true}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDegraded, resp.Status)
	assert.Equal(t, componentFail, resp.Components[componentBroker].Status)
	assert.Equal(t, "connection closed", resp.Components[componentBroker].Error)
	assert.Equal(t, componentOK, resp.Components[componentDB].Status)
}

func TestHealth_AllFail(t *testing.T) {
	h := Health(
		&stubDB{pingErr: errors.New("db down")},
		&stubStorage{healthErr: errors.New("s3 down")},
		&stubBroker{closed: true},
		healthLogger(),
	)

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDown, resp.Status)
	for name, comp := range resp.Components {
		assert.Equal(t, componentFail, comp.Status, "компонент %q должен быть fail", name)
		assert.NotEmpty(t, comp.Error)
	}
}

func TestHealth_NilPool(t *testing.T) {
	h := Health(nil, &stubStorage{}, &stubBroker{}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDegraded, resp.Status)
	assert.Equal(t, componentFail, resp.Components[componentDB].Status)
	assert.Equal(t, "pool is nil", resp.Components[componentDB].Error)
}

func TestHealth_NilStorage(t *testing.T) {
	h := Health(&stubDB{}, nil, &stubBroker{}, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, componentFail, resp.Components[componentS3].Status)
	assert.Equal(t, "storage is nil", resp.Components[componentS3].Error)
}

func TestHealth_NilBroker(t *testing.T) {
	h := Health(&stubDB{}, &stubStorage{}, nil, healthLogger())

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, componentFail, resp.Components[componentBroker].Status)
	assert.Equal(t, "broker is nil", resp.Components[componentBroker].Error)
}

func TestHealth_AllNil(t *testing.T) {
	// Хендлер без зависимостей не должен паниковать: все компоненты — fail.
	h := Health(nil, nil, nil, nil)

	rec, resp := doHealth(t, h)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, statusDown, resp.Status)
	require.Len(t, resp.Components, 3)
}

// TestHealth_Timeout проверяет, что зависшая проверка обрывается по таймауту,
// а не держит запрос бесконечно.
func TestHealth_Timeout(t *testing.T) {
	h := Health(&stubDB{blockUntilCtxDone: true}, &stubStorage{}, &stubBroker{}, healthLogger())

	start := time.Now()
	rec, resp := doHealth(t, h)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, componentFail, resp.Components[componentDB].Status)
	assert.Contains(t, resp.Components[componentDB].Error, context.DeadlineExceeded.Error())

	// Ответ пришёл примерно по таймауту: не раньше и не сильно позже.
	assert.GreaterOrEqual(t, elapsed, healthTimeout)
	assert.Less(t, elapsed, healthTimeout+2*time.Second)
}
