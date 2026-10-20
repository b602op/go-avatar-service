package metrics

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLazyPool создаёт пул без единого реального подключения: pgxpool открывает
// соединения лениво (MinConns = 0 в конфиге по умолчанию), а метрики читают
// только атомарный Stat(). Поэтому тест не требует поднятого Postgres —
// DSN намеренно указывает на заведомо закрытый порт.
func newLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig("postgres://avatar:avatar@127.0.0.1:1/avatars?sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

// gaugeValue скрейпит реестр и возвращает значение метрики по имени.
// Заодно проверяет, что серия ровно одна и без лейблов: метрики пула — это
// «одна метрика на процесс», лейблы только раздували бы кардинальность.
func gaugeValue(t *testing.T, gatherer prometheus.Gatherer, name string) float64 {
	t.Helper()

	families, err := gatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		assert.Equal(t, "GAUGE", family.GetType().String())

		metrics := family.GetMetric()
		require.Len(t, metrics, 1, "GaugeFunc должен давать одну серию")
		require.Empty(t, metrics[0].GetLabel(), "у метрик пула не должно быть лейблов")

		return metrics[0].GetGauge().GetValue()
	}

	t.Fatalf("метрика %s не зарегистрирована", name)

	return 0
}

// TestRegisterDBPoolMetrics проверяет связку GaugeFunc ↔ pgxpool.Stat():
// значения метрик совпадают со снимком пула на момент скрейпа.
func TestRegisterDBPoolMetrics(t *testing.T) {
	pool := newLazyPool(t)
	reg := prometheus.NewRegistry()
	m := NewSet(reg)
	m.RegisterDBPoolMetrics(pool)

	// Пул ленивый: соединений ещё нет, поэтому Stat() предсказуем и не меняется
	// между снимком и скрейпом.
	stat := pool.Stat()

	assert.Equal(t,
		float64(stat.TotalConns()),
		gaugeValue(t, reg, MetricDBPoolConnections),
	)
	assert.Equal(t,
		float64(stat.IdleConns()),
		gaugeValue(t, reg, MetricDBPoolConnectionsIdle),
	)
	assert.Equal(t,
		float64(stat.AcquiredConns()),
		gaugeValue(t, reg, MetricDBPoolConnectionsUsed),
	)

	// MaxConns — настроенная ёмкость пула, а не счётчик текущих соединений:
	// у ленивого пула она не нулевая, в отличие от остальных значений.
	maxConns := gaugeValue(t, reg, MetricDBPoolConnectionsMax)
	assert.Equal(t, float64(stat.MaxConns()), maxConns)
	assert.Positive(t, maxConns)
}
