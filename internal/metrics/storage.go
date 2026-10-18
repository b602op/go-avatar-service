package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// storageUpdateInterval — период пересчёта avatars_storage_bytes.
//
// 30 секунд — компромисс: агрегат нужен для дашборда «занято места», а не для
// алертов в реальном времени, поэтому лишний запрос к БД раз в полминуты дешевле,
// чем точность до секунды.
const storageUpdateInterval = 30 * time.Second

// storageQuery — суммарный размер активных аватарок.
//
// Мягко удалённые записи не считаем: их файлы удаляет воркер по событию, и в
// хранилище они не должны учитываться как занятое место.
const storageQuery = `SELECT COALESCE(SUM(size_bytes), 0) FROM avatars WHERE deleted_at IS NULL`

// StartStorageUpdater поддерживает AvatarsStorageBytes в актуальном состоянии.
//
// Первый пересчёт выполняется сразу при старте: иначе на графике был бы провал
// в storageUpdateInterval, а метрика выглядела бы как «хранилище пустое».
//
// Дальше значение обновляется раз в storageUpdateInterval в отдельной горутине —
// /metrics не должен ходить в БД на каждом скрейпе. Функция блокирующая:
// вызывать её нужно как `go StartStorageUpdater(...)`, остановка — по ctx.Done().
//
// Ошибка запроса не роняет процесс и не обнуляет метрику: в лог уходит Warn,
// а gauge сохраняет предыдущее значение.
func StartStorageUpdater(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	update := func() {
		var total int64
		if err := pool.QueryRow(ctx, storageQuery).Scan(&total); err != nil {
			log.WarnContext(ctx, "failed to query storage bytes", "error", err)
			return
		}
		AvatarsStorageBytes.Set(float64(total))
	}

	update()

	ticker := time.NewTicker(storageUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}
