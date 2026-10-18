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

## Kubernetes (iter3)

### ServiceMonitor не применён в локальном кластере (P2)

Требует Prometheus Operator (CRD `monitoring.coreos.com/v1`). Локально нода
minikube — 6 GB RAM, приложение и инфраструктура занимают ~1.5 GB, а стек
оператора (`kube-prometheus-stack`) требует ещё 2–3 GB, поэтому он не
устанавливается. Артефакт готов (`k8s/50-servicemonitor.yaml`), в проде —
применять после установки стека.

Альтернатива для локального стенда: статический scrape в `prometheus.yml`
(как в iter2) — джобы `server:9090` и `worker:9091` в
`docker/prometheus/prometheus.yml`; в кластере те же `/metrics` снимаются
`kubectl port-forward -n gophprofile svc/server 9090:9090`.

### postgres и rabbitmq: `capabilities.drop: [ALL]` без исключений не работает (P2, осознанно)

Проверено: с полным drop оба пода уходили в CrashLoopBackOff. У postgres в логах
`chmod: /var/lib/postgresql/data/pgdata: Operation not permitted` и
`find: ... Permission denied`, rabbitmq падал молча (`exitCode: 1`, пустые логи).
Причина одна: entrypoint обоих образов запускается от root, готовит том
(`chmod 00700 PGDATA`, `chown postgres` / `find /var/lib/rabbitmq ! -user rabbitmq
-exec chown`) и только потом сбрасывает привилегии (`gosu postgres` / `su-exec
rabbitmq`). Root без капабилити — обычный непривилегированный пользователь: прав
на чужие файлы у него не больше, чем у владельца, а смена uid требует CAP_SETUID.

Поэтому в `10-postgres.yaml` и `12-rabbitmq.yaml` возвращены пять капабилити:
`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETGID`, `SETUID`. Остальное из дефолтного
набора Docker (`NET_RAW`, `MKNOD`, `SETPCAP`, ...) остаётся выключенным, а
`allowPrivilegeEscalation: false` не даёт вернуть привилегии через setuid-бинарник
в образе. minio (pgsty/silo) работает с полным drop: он не сбрасывает привилегии
и остаётся root внутри контейнера.

Более чистое решение на будущее — запускать образы БД от их собственных
непривилегированных пользователей (`runAsUser: 70` для postgres, `100` для
rabbitmq): entrypoint сам пропускает подготовку тома и сброс привилегий, если
процесс уже не root, и капабилити не нужны вовсе. Не сделано, потому что требует
проверки прав на свежем томе для каждого образа, а выигрыш — только в отказе от
пяти капабилити, которые и так не дают выйти за пределы контейнера.

### worker: порт метрик открыт любому поду кластера (P3)

В `k8s/61-networkpolicy.yaml` у `worker-ingress` правило для порта 9091 задано без
`from` — то есть метрики воркера доступны из любого namespace. В проде источник
известен (Prometheus из namespace `monitoring`), поэтому правило нужно сузить до
`namespaceSelector` этого namespace.

Там же у `server-ingress` открыт только 8080: порт метрик сервера (9090) в
локальном кластере никто не скрейпит, а в проде его придётся добавить в
ingress-правило — иначе вместе с включением NetworkPolicy метрики сервера станут
недоступны.

### fsGroup не действует на hostPath-томах локального кластера (P3)

PV в minikube создаёт provisioner `k8s.io/minikube-hostpath`, а kubelet права на
hostPath-томах не переписывает: после применения `fsGroup: 1000` каталог
`/var/lib/postgresql/data` остался `root:root`, `pgdata` — `postgres:root 0700`.
Фактически fsGroup сработал только как дополнительная группа процесса
(`id` в поде показывает `groups=...1000`). В проде с CSI-провижнером kubelet
выставит группу и на файлах — тогда запись в том перестанет зависеть от uid
образа. Проверять это стоит после смены StorageClass.

### Секреты лежат в values.yaml открытым текстом (P2, iter3)

`helm/gophprofile/values.yaml` содержит учётные данные PostgreSQL, MinIO и
RabbitMQ. Для учебного стенда это удобно (один `helm install` поднимает всё), но
в проде секреты не должны жить в чарте: они попадают в values-файл, в историю
ревизий Helm (`helm get values`) и в Git.

План: поддержать `secrets.existingSecret` — если имя задано, чарт не создаёт
Secret, а ссылается на существующий; сами значения поставлять через External
Secrets / SOPS / Vault. Отдельно стоит учесть `RABBITMQ_URL` в ConfigMap: сейчас
он собирается из `secrets.rabbitmqUser/Password` (раньше в `k8s/` был
захардкожен `guest:guest` независимо от Secret), и при выносе секретов наружу
URL придётся либо тоже брать из Secret, либо собирать init-скриптом.

### k8s/ и helm/ дублируют манифесты (P2, iter3)

Сырые манифесты в `k8s/` и шаблоны чарта описывают один и тот же стек: правка
(например, таймаутов проб) требует синхронного изменения в двух местах, и
расхождение обнаружится только при деплое. Так уже разошлось содержимое
`61-networkpolicy.yaml`: в чарте добавлены `app: migration` в `postgres-ingress`
и отдельная политика `migration-egress`, которых в `k8s/` нет, — там нет и
самого Job'а миграций.

План: оставить один источник истины — либо генерировать `k8s/` из чарта
(`helm template`), либо удалить сырые манифесты и оставить чарт. Сейчас `k8s/`
сохранён для сравнения и для отладки без Helm.

### Хук миграций на upgrade берёт Secret предыдущей ревизии (P3, iter3)

`pre-upgrade` выполняется до применения объектов новой ревизии, поэтому Job
миграций читает ConfigMap и Secret в их текущем (старом) виде. Для миграций это
не критично — они зависят только от соединения с БД, — но если в upgrade меняются
учётные данные БД, миграция отработает со старым паролем. Варианты: переносить
хук на `post-upgrade` (тогда схема обновляется после старта новых подов, которые
мигрируют сами) или передавать Job'у параметры подключения явно, а не через
`envFrom`.