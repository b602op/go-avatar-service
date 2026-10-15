// Package migrator применяет SQL-миграции к базе данных при старте приложения.
package migrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/b602op/avatars-service/migrations"
)

// Up применяет все неприменённые миграции из встроенной ФС.
//
// Версии применённых миграций goose хранит в таблице goose_db_version, поэтому
// повторный запуск (в том числе параллельный старт server и worker) безопасен:
// уже применённые миграции пропускаются, а не выполняются второй раз.
//
// Функция вызывается до начала обслуживания запросов, поэтому схема
// гарантированно готова к первому обращению к БД.
func Up(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	// goose работает через database/sql, а приложение — на pgxpool.
	// OpenDBFromPool — мост между ними: он не забирает соединения у пула
	// (MaxIdleConns = 0) и не закрывает сам пул при вызове Close().
	db := stdlib.OpenDBFromPool(pool)
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("failed to close migration db handle", "error", err)
		}
	}()

	// Session locker берёт session-level advisory-лок на время прогона.
	// Без него одновременный старт server и worker мог бы привести к тому,
	// что оба процесса начнут применять одни и те же миграции параллельно:
	// goose по умолчанию блокировку не ставит.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migrator: create session locker: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations.FS,
		goose.WithSessionLocker(locker),
		goose.WithSlog(log),
	)
	if err != nil {
		return fmt.Errorf("migrator: create provider: %w", err)
	}
	// Provider.Close закрывает db, поэтому вызывается раньше отложенного
	// db.Close() (LIFO). Повторное закрытие database/sql безопасно.
	defer func() {
		if err := provider.Close(); err != nil {
			log.Error("failed to close migration provider", "error", err)
		}
	}()

	// Если новых миграций нет, Up возвращает пустой список и nil.
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrator: apply migrations: %w", err)
	}

	for _, r := range results {
		log.Info("migration applied", "migration", r.String())
	}

	return nil
}
