package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/metrics"
)

// testMetricsServer поднимает хендлер сервера метрик на случайном порту.
// Сам *http.Server здесь не нужен: тестируется маршрутизация /metrics и /health.
func testMetricsServer(t *testing.T, withHealth bool, m *metrics.Set) *httptest.Server {
	t.Helper()

	srv := NewMetricsServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)), withHealth, m)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	return ts
}

// get выполняет GET и возвращает код и тело ответа.
func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body)
}

// TestMetricsServer_ExposesMetrics проверяет, что /metrics отдаёт метрики в
// формате Prometheus, включая наши HTTP-метрики.
func TestMetricsServer_ExposesMetrics(t *testing.T) {
	m := metrics.NewSet(prometheus.NewRegistry())
	ts := testMetricsServer(t, false, m)

	// Векторные метрики попадают в выдачу только после первого наблюдения:
	// без этого /metrics содержал бы лишь runtime-метрики Go.
	m.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/test", "200").Inc()
	m.HTTPRequestDuration.WithLabelValues(http.MethodGet, "/test", "200").Observe(0.01)

	status, body := get(t, ts, "/metrics")

	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "# TYPE "+metrics.MetricHTTPRequestsTotal+" counter")
	assert.Contains(t, body, "# TYPE "+metrics.MetricHTTPRequestDuration+" histogram")
}

// TestMetricsServer_Health проверяет liveness-эндпоинт воркера.
func TestMetricsServer_Health(t *testing.T) {
	ts := testMetricsServer(t, true, metrics.NewSet(prometheus.NewRegistry()))

	status, body := get(t, ts, "/health")

	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body)
}

// TestMetricsServer_HealthDisabled: серверу (не воркеру) /health не нужен —
// у него уже есть полноценный /health в основном роутере, проверяющий
// зависимости. Вспомогательный сервер не должен его подменять.
func TestMetricsServer_HealthDisabled(t *testing.T) {
	ts := testMetricsServer(t, false, metrics.NewSet(prometheus.NewRegistry()))

	status, _ := get(t, ts, "/health")

	assert.Equal(t, http.StatusNotFound, status)
}
