# Технический долг

Осознанные отступления от ТЗ и задачи, которые приняты сейчас и закрываются
позже. Формат записи: **что не сделано (приоритет)** — причина и план.

## Observability (iter2)

### SQL-логи pgx не пишутся (P3)

В `internal/repository/postgres/db.go` у пула задан только
`cfg.ConnConfig.Tracer = otelpgx.NewTracer()`. В pgx v5 поля `Logger` в
`ConnConfig` нет — SQL-логи включаются отдельным трейсером `tracelog`, а слот
`Tracer` уже занят `otelpgx`. Чтобы получить и спаны БД, и SQL-логи в общем
JSON-потоке, нужен `multitracer.New(otelpgx.NewTracer(), tracelog.NewTraceLog(...))`
со slog-адаптером на `tracelog.LoggerFunc`.

Отложено осознанно: спаны БД в Jaeger (`db.system.name`, `db.query.text`,
`db.operation.name`) закрывают потребность в отладке запросов, а дублирование
SQL в логах добавило бы шум в Loki.

### Используются актуальные семконы OTel, а не имена из ТЗ (осознанно)

В ТЗ приведены устаревшие имена атрибутов. В коде — актуальные:

| ТЗ | В коде |
|---|---|
| `db.system` | `db.system.name` (= `postgresql`), `db.operation.name`, `db.query.text` |
| `s3.bucket`, `s3.key` | `aws.s3.bucket`, `aws.s3.key` |
| `messaging.destination` | `messaging.destination.name`, `messaging.rabbitmq.destination.routing_key`, `messaging.message.id` |

Изменение не требуется: это не долг, а зафиксированное решение — устаревшие
имена несовместимы с актуальными дашбордами и коллекторами. Значения
параметров SQL в трейсы не попадают: `otelpgx.WithIncludeQueryParameters()`
намеренно не включён, в `db.query.text` уходит шаблон с плейсхолдерами
(PII — `user_id`, ключи объектов — в трейсах не нужны).

### `avatars_storage_bytes` без разреза по `user_id` (P3)

Метрика отдаёт только суммарный объём активных аватарок. Per-user разрез
отложен из-за кардинальности: `user_id` уникален для каждого клиента, и число
серий росло бы вместе с базой пользователей. Если понадобится — делать не
метку, а отдельный запрос с агрегацией/top-N (по аналогии с
`StartStorageUpdater`), а не второй `GaugeVec`.

### Тесты S3-клиента не используют testcontainers (P2)

`internal/services/s3` в тестах ходит на `S3_ENDPOINT` (по умолчанию
`localhost:9000`), то есть требует поднятого compose — при недоступном
хранилище тесты не скипаются, а падают. `internal/services/publisher` уже
работает через testcontainers (`rabbitmq:3-management-alpine`), как и
`internal/worker`.

План: поднимать `pgsty/silo` через testcontainers в `TestMain` и передавать
endpoint в конфиг — так же, как это сделано в worker-тестах.

### Тестовая БД `avatars_test` создаётся вручную (P2)

`internal/repository/postgres/avatar_test.go` подключается к
`postgres://avatar:avatar@localhost:5432/avatars_test` и в сообщении об ошибке
просит «запусти `make up` и создай avatars_test». Сама БД не создаётся ничем:
в compose нет init-скрипта, поэтому на чистом окружении тесты пакета падают.

План: `docker-entrypoint-initdb.d/01-create-test-db.sql` с
`CREATE DATABASE avatars_test;` и volume с ним в сервис `postgres`.

### `/health` не трейсится (осознанно)

В `internal/api/router.go` стоит `otelhttp.WithFilter`, отсекающий `/health`:
Docker healthcheck обращается к нему каждые 15 секунд, и трейсы заполнялись бы
служебным шумом. В метриках `/health` остаётся
(`http_requests_total{path="/health"}`) — по нему видно, что контейнер живой.

### `middleware.RealIP` без списка доверенных прокси (P3)

`internal/api/router.go` использует `chi/middleware.RealIP`, помеченный
deprecated (GHSA-3fxj-6jh8-hvhx и др.): без списка доверенных прокси
`X-Forwarded-For` / `X-Real-IP` можно подделать, и в лог попадёт чужой
`remote_addr` — то есть ломается как раз observability-поле.

Риск ограничен: `RemoteAddr` используется только для логирования, не для
авторизации и лимитов. План — доверять заголовкам лишь от известных
прокси-адресов (отдельная настройка).

### Публикация событий не переподключается к брокеру (P2)

`internal/services/publisher/rabbitmq.go` создаёт AMQP-соединение один раз в
`NewRabbitMQ` и не восстанавливает его: `NotifyClose` не используется, реконнекта
нет. После перезапуска `rabbitmq` соединение остаётся закрытым до перезапуска
процесса, и `/health` бессрочно отвечает `503` с `broker: connection closed` —
контейнер `server` уходит в `unhealthy` и сам не восстанавливается, продолжая
при этом обслуживать запросы.

У воркера та же проблема, но он восстанавливается сам: закрытие канала доставки
завершает `Consumer.Run` ошибкой, `Worker.Run` её возвращает, и `cmd/worker`
делает `os.Exit(1)` — процесс поднимает `restart: unless-stopped`. То есть ценой
перезапуска процесса, а не переподключением на ходу.

Обнаружено при проверке метрик RabbitMQ: достаточно было пересоздать контейнер
брокера. План — обернуть соединение в переподключение (например, `amqp091-go`
`NotifyClose` + повторная публикация с backoff, как в `worker/consumer.go`).


## Observability (iter2) — замечания ревью

- **Alertmanager не настроен** (P2, бонус ТЗ)
  Правила HighErrorRate/HighResponseTime не реализованы.
  Ревьюер рекомендовал добавить. Отложено.

- **Метрики через глобальные переменные** (P2)
  `internal/metrics/*` использует пакетные `var` + `promauto`.
  Ревьюер рекомендовал передавать коллекторы через структуру
  (`type Metrics struct { ... }`, `NewMetrics(reg prometheus.Registerer)`),
  чтобы зависимости были явными и метрики можно было инжектить в тестах.