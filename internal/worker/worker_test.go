package worker

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/metrics"
)

// testMetrics возвращает набор метрик с отдельным реестром: тесты не делят
// наблюдения друг с другом и не трогают глобальный реестр Prometheus.
func testMetrics() *metrics.Set {
	return metrics.NewSet(prometheus.NewRegistry())
}

// TestWorker_GracefulShutdown проверяет, что Run возвращается при отмене контекста.
func TestWorker_GracefulShutdown(t *testing.T) {
	env, ctx := setupTest(t)

	// Объявляем очереди (их создаёт паблишер) — иначе consumer'у нечего слушать.
	newTestPublisher(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, testLogger(), testMetrics())
	w, err := NewWorker(config.RabbitMQConfig{
		URL:      rabbitURL,
		Exchange: testExchange,
		RetryTTL: testRetryTTL,
	}, h, nil, testLogger())
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// Даём consumer'ам подняться, затем инициируем graceful shutdown.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker не остановился за 5 секунд после отмены контекста")
	}
}

// TestWorker_Close_Idempotent проверяет, что повторное закрытие безопасно.
func TestWorker_Close_Idempotent(t *testing.T) {
	env, _ := setupTest(t)
	newTestPublisher(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, testLogger(), testMetrics())
	w, err := NewWorker(config.RabbitMQConfig{
		URL:      rabbitURL,
		Exchange: testExchange,
		RetryTTL: testRetryTTL,
	}, h, nil, testLogger())
	require.NoError(t, err)

	require.NoError(t, w.Close())
	require.NoError(t, w.Close())
}
