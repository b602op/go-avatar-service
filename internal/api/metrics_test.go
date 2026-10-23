package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/metrics"
)

// newTestMetrics создаёт изолированный набор метрик: каждый тест пишет в свой
// реестр и не зависит от порядка выполнения соседних тестов.
func newTestMetrics(t *testing.T) *metrics.Set {
	t.Helper()
	return metrics.NewSet(prometheus.NewRegistry())
}

// metricsRouter собирает минимальный Chi-роутер с middleware Metrics.
// otelhttp здесь не нужен: проверяются метки метрик, а не трейсинг.
func metricsRouter(m *metrics.Set) *chi.Mux {
	r := chi.NewRouter()
	r.Use(Metrics(m))
	r.Get("/api/v1/users/{user_id}/avatar", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return r
}

// requestsTotal читает текущее значение счётчика запросов по меткам.
func requestsTotal(t *testing.T, m *metrics.Set, method, path, status string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.HTTPRequestsTotal.WithLabelValues(method, path, status))
}

// TestMetrics_UsesRoutePattern проверяет главное требование к middleware:
// в метку path попадает ШАБЛОН маршрута, а не конкретный URL. Иначе каждая
// аватарка создавала бы новую временную серию и кардинальность росла бы
// вместе с данными.
func TestMetrics_UsesRoutePattern(t *testing.T) {
	m := newTestMetrics(t)
	router := metricsRouter(m)

	const pattern = "/api/v1/users/{user_id}/avatar"
	before := requestsTotal(t, m, http.MethodGet, pattern, "404")

	// Два разных URL одного маршрута — должны попасть в одну серию.
	for _, url := range []string{"/api/v1/users/user-1/avatar", "/api/v1/users/user-2/avatar"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, url, nil))
	}

	assert.Equal(t, 2.0, requestsTotal(t, m, http.MethodGet, pattern, "404")-before,
		"оба URL должны попасть в серию с шаблоном пути")
	assert.Equal(t, 0.0, requestsTotal(t, m, http.MethodGet, "/api/v1/users/user-1/avatar", "404"),
		"конкретный URL не должен становиться меткой path")
}

// TestMetrics_UnknownRouteFallback проверяет fallback: у запроса вне дерева
// маршрутов шаблона нет, поэтому path = "unknown".
func TestMetrics_UnknownRouteFallback(t *testing.T) {
	m := newTestMetrics(t)
	router := metricsRouter(m)

	before := requestsTotal(t, m, http.MethodGet, unknownRoute, "404")
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no-such-route", nil))

	assert.Equal(t, 1.0, requestsTotal(t, m, http.MethodGet, unknownRoute, "404")-before)
}

// TestMetrics_StatusWithoutWriteHeader проверяет нормализацию статуса: хендлер,
// который ничего не пишет, net/http отдаёт как 200, поэтому в метрике не должно
// оказаться 0.
func TestMetrics_StatusWithoutWriteHeader(t *testing.T) {
	m := newTestMetrics(t)
	router := chi.NewRouter()
	router.Use(Metrics(m))
	router.Get("/empty", func(http.ResponseWriter, *http.Request) {})

	before := requestsTotal(t, m, http.MethodGet, "/empty", "200")
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/empty", nil))

	assert.Equal(t, 1.0, requestsTotal(t, m, http.MethodGet, "/empty", "200")-before)
}

// TestMetrics_ObservesDuration проверяет, что middleware пишет и гистограмму:
// без неё в RED-наборе не хватает duration.
func TestMetrics_ObservesDuration(t *testing.T) {
	m := newTestMetrics(t)
	router := metricsRouter(m)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/users/u/avatar", nil))

	require.GreaterOrEqual(t, testutil.CollectAndCount(m.HTTPRequestDuration), 1,
		"гистограмма длительности должна содержать наблюдения")
}
