package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool создаёт пул подключений к PostgreSQL и пингует его.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	// Трейсинг SQL: pgx сам прокидывает context запроса в TraceQueryStart, поэтому
	// спаны БД становятся дочерними к спанам HTTP-запроса (context propagation
	// обеспечивается otelpgx, ручной работы не требуется).
	//
	// Опции осознанно не передаются, в частности НЕ включаем
	// otelpgx.WithIncludeQueryParameters(): по умолчанию в атрибут с текстом
	// запроса (db.query.text по актуальным семконам) попадает шаблон с
	// плейсхолдерами ($1, $2), а не значения. Реальные параметры — это PII
	// (user_id, ключи объектов в S3), в трейсы они попадать не должны.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return pool, nil
}
