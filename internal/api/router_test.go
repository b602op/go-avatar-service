package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/handlers"
	"github.com/b602op/avatars-service/internal/httpx"
)

// apiStubService — минимальный стаб handlers.AvatarService для тестов роутера.
// Все операции возвращают ErrNotFound: проверяется маршрутизация, не бизнес-логика.
type apiStubService struct{}

func (apiStubService) Upload(context.Context, string, string, string, []byte) (*domain.Avatar, error) {
	return nil, domain.ErrNotFound
}

func (apiStubService) GetByID(context.Context, uuid.UUID) (*domain.Avatar, error) {
	return nil, domain.ErrNotFound
}

func (apiStubService) ListByUserID(context.Context, string) ([]*domain.Avatar, error) {
	return nil, domain.ErrNotFound
}

func (apiStubService) GetContent(context.Context, uuid.UUID, string) (io.ReadCloser, *domain.ObjectInfo, error) {
	return nil, nil, domain.ErrNotFound
}

func (apiStubService) Delete(context.Context, uuid.UUID, string) error {
	return domain.ErrNotFound
}

// healthStubHandler — простой /health для тестов маршрутизации: всегда 200.
func healthStubHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// newRouterWithHandler собирает роутер с реальным хендлером на стабе сервиса.
func newRouterWithHandler() *chi.Mux {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(RouterDeps{
		Log:            log,
		AvatarHandler:  handlers.NewAvatarHandler(apiStubService{}, log),
		Health:         healthStubHandler(),
		AllowedOrigins: []string{"*"},
	})
}

// newTestRouter собирает защищённый роутер с хендлером, читающим userID из контекста.
func newTestRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(UserID())
	r.Get("/protected", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := httpx.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "no userID", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(userID))
	})
	return r
}

func TestRouter_Health(t *testing.T) {
	router := NewRouter(RouterDeps{Health: healthStubHandler()})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

// TestRouter_Health_NilHandler проверяет, что роутер без переданного хендлера
// /health не паникует, а честно сообщает о нерабочих зависимостях.
func TestRouter_Health_NilHandler(t *testing.T) {
	router := NewRouter(RouterDeps{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var resp handlers.HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "down", resp.Status)
}

func TestRouter_NotFound(t *testing.T) {
	router := NewRouter(RouterDeps{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unknown", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRouter_StaticHTML(t *testing.T) {
	// Статика раздаётся относительно корня проекта; из пакета internal/api
	// рабочая директория — сам пакет, поэтому поднимаемся на два уровня.
	t.Chdir("../../")

	router := NewRouter(RouterDeps{})

	// http.FileServer канонизирует путь .../index.html → .../ (301 Moved
	// Permanently) — стандартное поведение net/http. Проверяем сам редирект.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/web/static/index.html", nil))
	require.Equal(t, http.StatusMovedPermanently, rec.Code)

	// Канонический путь отдаёт страницу с кодом 200.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/web/static/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Avatar Upload Service")
}

func TestMiddleware_UserID_Valid(t *testing.T) {
	router := newTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set(userIDHeader, "user-123")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "user-123", rec.Body.String())
}

func TestMiddleware_UserID_Missing(t *testing.T) {
	router := newTestRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/protected", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid X-User-ID"}`, rec.Body.String())
}

func TestMiddleware_UserID_TooLong(t *testing.T) {
	router := newTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set(userIDHeader, strings.Repeat("a", maxUserIDLen+1))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMiddleware_UserID_InvalidChars(t *testing.T) {
	router := newTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set(userIDHeader, "user@123")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMiddleware_RequestLogger(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(RequestLogger(log))
	router.Get("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Успешный запрос логируется на уровне INFO.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	var okEntry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &okEntry))
	assert.Equal(t, "INFO", okEntry["level"])
	assert.Equal(t, "http request", okEntry["msg"])
	assert.Equal(t, float64(http.StatusOK), okEntry["status"])

	// Серверная ошибка — на уровне ERROR.
	buf.Reset()
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	var errEntry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &errEntry))
	assert.Equal(t, "ERROR", errEntry["level"])
	assert.Equal(t, float64(http.StatusInternalServerError), errEntry["status"])
}

func TestRouter_APIRequiresUserID(t *testing.T) {
	router := newRouterWithHandler()

	// Без заголовка X-User-ID защищённый маршрут отдаёт 400 от middleware.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/avatars/"+uuid.NewString(), nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid X-User-ID"}`, rec.Body.String())
}

func TestRouter_APIAcceptsUserID(t *testing.T) {
	router := newRouterWithHandler()

	// С валидным X-User-ID запрос доходит до хендлера (стаб отдаёт ErrNotFound → 404).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/avatars/"+uuid.NewString(), nil)
	req.Header.Set(userIDHeader, "user-123")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusBadRequest, rec.Code, "запрос не должен падать на валидации заголовка")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRouter_UploadRouteContract(t *testing.T) {
	router := newRouterWithHandler()

	// Контракт фронтенда: POST /api/v1/avatars БЕЗ завершающего слэша.
	// Проверяем, что маршрут существует и не редиректит (иначе был бы 301/404).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/avatars", nil)
	req.Header.Set(userIDHeader, "user-123")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusNotFound, rec.Code, "маршрут POST /api/v1/avatars должен существовать")
	assert.NotEqual(t, http.StatusMovedPermanently, rec.Code, "маршрут не должен редиректить на слэш")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "пустое тело → 400 Invalid multipart form")
}
