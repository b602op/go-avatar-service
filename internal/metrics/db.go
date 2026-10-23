package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Имена метрик пула БД. Лейблов у них нет: пул в процессе один, а
// user_id/instance в лейблах только раздували бы кардинальность.
const (
	// MetricDBPoolConnections — всего соединений в пуле (занятые + idle).
	MetricDBPoolConnections = "db_pool_connections"
	// MetricDBPoolConnectionsIdle — соединения, готовые к выдаче.
	MetricDBPoolConnectionsIdle = "db_pool_connections_idle"
	// MetricDBPoolConnectionsUsed — соединения, выданные в работу.
	MetricDBPoolConnectionsUsed = "db_pool_connections_used"
	// MetricDBPoolConnectionsMax — верхняя граница пула (cfg.MaxConns).
	MetricDBPoolConnectionsMax = "db_pool_connections_max"
)

// RegisterDBPoolMetrics регистрирует метрики состояния пула подключений.
//
// Используется GaugeFunc, а не периодический опрос: callback вызывается самим
// Prometheus при скрейпе, а pgxpool.Stat() читает атомарные счётчики пула —
// это O(1) без блокировок и без фоновой горутины.
//
// Метод рассчитан на однократный вызов на процесс: повторная регистрация тех же
// имён в том же реестре — ошибка программиста (promauto паникует).
func (m *Set) RegisterDBPoolMetrics(pool *pgxpool.Pool) {
	// Stat() дёргается на каждый скрейп и каждый раз берёт свежий снимок,
	// поэтому отдельный кэш или таймер не нужен.
	stat := pool.Stat
	factory := promauto.With(m.reg)

	factory.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricDBPoolConnections,
		Help: "Total number of connections in the pool.",
	}, func() float64 {
		return float64(stat().TotalConns())
	})

	factory.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricDBPoolConnectionsIdle,
		Help: "Number of idle connections in the pool.",
	}, func() float64 {
		return float64(stat().IdleConns())
	})

	factory.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricDBPoolConnectionsUsed,
		Help: "Number of connections currently acquired from the pool.",
	}, func() float64 {
		return float64(stat().AcquiredConns())
	})

	factory.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricDBPoolConnectionsMax,
		Help: "Maximum number of connections the pool can hold.",
	}, func() float64 {
		return float64(stat().MaxConns())
	})
}
