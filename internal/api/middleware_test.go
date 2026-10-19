package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCORS_AllowAll(t *testing.T) {
	handler := CORS([]string{"*"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://example.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Whitelist_Allowed(t *testing.T) {
	handler := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "http://localhost:3000", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Whitelist_Forbidden(t *testing.T) {
	handler := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://evil.com")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Preflight(t *testing.T) {
	handler := CORS([]string{"*"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight не должен доходить до хендлера")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/avatars", nil)
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST, DELETE, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Content-Type, X-User-ID", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "86400", rec.Header().Get("Access-Control-Max-Age"))
}

// okHandler отвечает 200 и считает, сколько раз до него дошёл запрос.
func okHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.WriteHeader(http.StatusOK)
	})
}

func TestRateLimit_DisabledOnZeroRPS(t *testing.T) {
	var calls int
	handler := RateLimit(0, 0)(okHandler(&calls))

	// Больше, чем любой burst: при выключенном лимите все запросы проходят.
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Equal(t, 10, calls)
}

func TestRateLimit_AllowsWithinBurst(t *testing.T) {
	var calls int
	// rps = 1: за время теста bucket не успевает пополниться, поэтому все
	// запросы идут строго из burst.
	handler := RateLimit(1, 3)(okHandler(&calls))

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Equal(t, 3, calls)
}

func TestRateLimit_RejectsOverBurst(t *testing.T) {
	var calls int
	handler := RateLimit(1, 2)(okHandler(&calls))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	}

	// Третья попытка исчерпывает burst: до хендлера запрос не доходит.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "Too Many Requests")
	assert.Equal(t, 2, calls, "отклонённый запрос не должен доходить до хендлера")
}

// TestRateLimit_ServicePathsExempt проверяет, что пробы kubelet не упираются в
// лимит: 429 на /live означал бы перезапуск здорового пода при всплеске
// трафика, то есть лимитер сам стал бы источником отказов.
func TestRateLimit_ServicePathsExempt(t *testing.T) {
	var calls int
	handler := RateLimit(1, 1)(okHandler(&calls))

	// Первый запрос исчерпывает burst.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/avatars", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Пользовательский путь теперь отклоняется...
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/avatars", nil))
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	// ...а служебные — нет, сколько бы их ни пришло.
	for _, path := range []string{"/live", "/health"} {
		for i := 0; i < 5; i++ {
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rec.Code, "путь %s не должен лимитироваться", path)
		}
	}
	assert.Equal(t, 11, calls)
}

func TestIsServicePath(t *testing.T) {
	assert.True(t, isServicePath("/live"))
	assert.True(t, isServicePath("/health"))

	// Пользовательский трафик лимитируется и трейсится.
	assert.False(t, isServicePath("/"))
	assert.False(t, isServicePath("/api/v1/avatars"))
	assert.False(t, isServicePath("/web/upload"))
	assert.False(t, isServicePath("/live/extra"))
	assert.False(t, isServicePath("/healthz"))
}
