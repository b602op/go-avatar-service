// Package s3 реализует клиент S3-совместимого хранилища (MinIO/Silo).
//
// Клиент — тонкая обёртка над minio-go/v7: подключение, идемпотентное создание
// бакета, операции над объектами и health-проверка. Бизнес-логика (генерация
// ключей, валидация MIME и т.п.) сюда не входит — это ответственность
// сервисного слоя.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services/circuitbreaker"
)

// defaultContentType применяется, когда вызывающая сторона не задала MIME-тип.
const defaultContentType = "application/octet-stream"

// tracer — трейсер для ручных спанов S3: у minio-go нет готовой OTel-инструментации,
// поэтому спаны операций создаются вручную.
//
// Спаны помечаются как SpanKindClient: каждая операция — исходящий вызов
// внешнего хранилища, и в трейсе это отдельный узел рядом с SQL-спанами.
//
// Переменная инициализируется при загрузке пакета, то есть до telemetry.Init.
// Это безопасно: глобальный провайдер OTel отдаёт до установки SDK no-op трейсеры
// и подменяет их на трейсеры SDK в момент otel.SetTracerProvider.
var tracer = otel.Tracer("github.com/b602op/avatars-service/s3")

// Проверка на этапе компиляции, что Client реализует доменный интерфейс.
var _ domain.ObjectStorage = (*Client)(nil)

// Client — обёртка над minio-go/v7 для работы с S3-совместимым хранилищем.
type Client struct {
	mc     *minio.Client
	bucket string
	region string
	log    *slog.Logger

	// breaker защищает операции над объектами от вызовов к лежащему хранилищу.
	//
	// Оборачиваются только операции с объектами (Upload, Download, StatObject,
	// Delete, DeleteMany). EnsureBucket и HealthCheck намеренно остаются вне
	// breaker'а: они отвечают на вопрос «доступно ли хранилище вообще», и ответ
	// «breaker открыт» здесь был бы подменой реального состояния зависимости —
	// ровно то, что /health должен показывать честно.
	breaker *circuitbreaker.Breaker
}

// New создаёт клиент S3.
//
// Бакет НЕ создаётся: для этого есть отдельный метод EnsureBucket, который
// вызывается при старте приложения. Подключение к хранилищу также не проверяется —
// это делает HealthCheck или первый реальный вызов.
func New(cfg config.S3Config, log *slog.Logger) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: create client: %w", err)
	}

	if log == nil {
		log = slog.Default()
	}

	log.Debug("s3 client initialized",
		"endpoint", cfg.Endpoint,
		"bucket", cfg.Bucket,
		"secure", cfg.UseSSL,
	)

	return &Client{
		mc:     mc,
		bucket: cfg.Bucket,
		region: cfg.Region,
		log:    log,
		breaker: circuitbreaker.New("s3",
			// Отсутствие объекта — штатный ответ (аватарку могли удалить), а не
			// деградация хранилища: без этого исключения пять подряд запросов к
			// удалённым аватарам открывали бы breaker и роняли бы весь S3-путь.
			circuitbreaker.WithIsExcluded(isNotFound),
			circuitbreaker.LogStateChanges(log),
		),
	}, nil
}

// EnsureBucket создаёт бакет, если его ещё нет.
//
// Метод идемпотентен: повторный вызов на существующем бакете не является ошибкой.
func (c *Client) EnsureBucket(ctx context.Context) (err error) {
	ctx, span := tracer.Start(ctx, "S3.EnsureBucket",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.AWSS3Bucket(c.bucket)),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("s3: check bucket %q: %w", c.bucket, err)
	}
	if exists {
		c.log.DebugContext(ctx, "s3 bucket already exists", "bucket", c.bucket)
		return nil
	}

	if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{Region: c.region}); err != nil {
		// Гонка: параллельный вызов мог создать бакет между проверкой и созданием.
		// Если он теперь существует — это не ошибка.
		if exists, checkErr := c.mc.BucketExists(ctx, c.bucket); checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("s3: create bucket %q: %w", c.bucket, err)
	}

	c.log.InfoContext(ctx, "s3 bucket created", "bucket", c.bucket)
	return nil
}

// Upload загружает объект по ключу.
//
// size должен быть точным размером данных: потоковая загрузка неизвестной длины
// (size = -1) не поддерживается. Если contentType пустой, применяется
// "application/octet-stream".
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (err error) {
	ctx, span := tracer.Start(ctx, "S3.Upload",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.AWSS3Bucket(c.bucket),
			semconv.AWSS3Key(key),
			attribute.Int64("s3.size", size),
			attribute.String("s3.content_type", contentType),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if size < 0 {
		return fmt.Errorf("s3: upload %q: exact size is required, got %d", key, size)
	}
	if contentType == "" {
		contentType = defaultContentType
	}

	_, err = circuitbreaker.Execute(c.breaker, func() (minio.UploadInfo, error) {
		return c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{
			ContentType: contentType,
		})
	})
	if err != nil {
		c.log.DebugContext(ctx, "s3 upload failed", "key", key, "error", err)
		return fmt.Errorf("s3: upload %q: %w", key, err)
	}
	return nil
}

// Download возвращает поток чтения и метаданные объекта.
//
// Если объекта нет — domain.ErrNotFound. Вызывающая сторона обязана закрыть
// возвращённый io.ReadCloser.
func (c *Client) Download(ctx context.Context, key string) (rc io.ReadCloser, info *domain.ObjectInfo, err error) {
	ctx, span := tracer.Start(ctx, "S3.Download",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.AWSS3Bucket(c.bucket),
			semconv.AWSS3Key(key),
		),
	)
	defer func() {
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	// GetObject ленив: HTTP-запроса при его вызове нет, поэтому в breaker
	// оборачиваются оба шага — сам GetObject и обязательный Stat, который и
	// выполняет запрос к хранилищу. Иначе breaker не увидел бы реальных сбоев.
	dl, err := circuitbreaker.Execute(c.breaker, func() (downloadResult, error) {
		obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return downloadResult{}, err
		}

		stat, err := obj.Stat()
		if err != nil {
			_ = obj.Close()
			return downloadResult{}, err
		}
		return downloadResult{obj: obj, stat: stat}, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("s3: download %q: %w", key, err)
	}

	return dl.obj, toObjectInfo(dl.stat), nil
}

// downloadResult — результат успешного Download: поток чтения и метаданные.
// Нужен как единый тип значения для circuitbreaker.Execute, чтобы открытый
// breaker не отдавал наружу полупустой результат.
type downloadResult struct {
	obj  *minio.Object
	stat minio.ObjectInfo
}

// StatObject возвращает метаданные объекта. Если объекта нет — domain.ErrNotFound.
func (c *Client) StatObject(ctx context.Context, key string) (info *domain.ObjectInfo, err error) {
	ctx, span := tracer.Start(ctx, "S3.StatObject",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.AWSS3Bucket(c.bucket),
			semconv.AWSS3Key(key),
		),
	)
	defer func() {
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	stat, err := circuitbreaker.Execute(c.breaker, func() (minio.ObjectInfo, error) {
		return c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	})
	if err != nil {
		if isNotFound(err) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("s3: stat %q: %w", key, err)
	}
	return toObjectInfo(stat), nil
}

// Delete удаляет объект.
//
// Отсутствие объекта не является ошибкой: S3 отвечает успехом на удаление
// несуществующего ключа, поэтому метод идемпотентен.
func (c *Client) Delete(ctx context.Context, key string) (err error) {
	ctx, span := tracer.Start(ctx, "S3.Delete",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.AWSS3Bucket(c.bucket),
			semconv.AWSS3Key(key),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if _, err = circuitbreaker.Execute(c.breaker, func() (struct{}, error) {
		return struct{}{}, c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
	}); err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// DeleteMany удаляет несколько объектов одним batch-запросом.
//
// Ошибки отдельных ключей логируются и объединяются в одну ошибку, но не
// прерывают batch: так за один вызов удаляется максимум объектов.
func (c *Client) DeleteMany(ctx context.Context, keys []string) (err error) {
	ctx, span := tracer.Start(ctx, "S3.DeleteMany",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.AWSS3Bucket(c.bucket),
			// Только количество ключей: сами ключи раздули бы спан, а как
			// атрибут с высокой кардинальностью были бы бесполезны в выборках.
			attribute.Int("s3.key_count", len(keys)),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if len(keys) == 0 {
		return nil
	}

	objectsCh := make(chan minio.ObjectInfo, len(keys))
	go func() {
		defer close(objectsCh)
		for _, key := range keys {
			objectsCh <- minio.ObjectInfo{Key: key}
		}
	}()

	// RemoveObjects ленив: ошибки по отдельным ключам приходят не из возврата
	// вызова, а из канала результатов. Поэтому в breaker оборачивается весь
	// разбор канала, а агрегированная ошибка возвращается наружу как результат —
	// иначе breaker никогда не увидел бы сбоев batch-удаления.
	_, err = circuitbreaker.Execute(c.breaker, func() ([]error, error) {
		var errs []error
		for res := range c.mc.RemoveObjects(ctx, c.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
			c.log.WarnContext(ctx, "s3 batch delete failed", "key", res.ObjectName, "error", res.Err)
			errs = append(errs, fmt.Errorf("delete %q: %w", res.ObjectName, res.Err))
		}
		return errs, errors.Join(errs...)
	})
	return err
}

// HealthCheck проверяет доступность хранилища и наличие бакета.
// Используется в /health (ШАГ 8).
func (c *Client) HealthCheck(ctx context.Context) (err error) {
	ctx, span := tracer.Start(ctx, "S3.HealthCheck",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.AWSS3Bucket(c.bucket)),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("s3: health check: %w", err)
	}
	if !exists {
		return fmt.Errorf("s3: health check: bucket %q does not exist", c.bucket)
	}
	return nil
}

// toObjectInfo переводит метаданные minio в доменную структуру.
func toObjectInfo(info minio.ObjectInfo) *domain.ObjectInfo {
	return &domain.ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		ContentType:  info.ContentType,
		ETag:         info.ETag,
		LastModified: info.LastModified,
	}
}

// isNotFound определяет, что ошибка означает отсутствие объекта или бакета.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, domain.ErrNotFound) {
		return true
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return true
	default:
		return false
	}
}
