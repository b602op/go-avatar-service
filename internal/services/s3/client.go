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

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
)

// defaultContentType применяется, когда вызывающая сторона не задала MIME-тип.
const defaultContentType = "application/octet-stream"

// Проверка на этапе компиляции, что Client реализует доменный интерфейс.
var _ domain.ObjectStorage = (*Client)(nil)

// Client — обёртка над minio-go/v7 для работы с S3-совместимым хранилищем.
type Client struct {
	mc     *minio.Client
	bucket string
	region string
	log    *slog.Logger
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
	}, nil
}

// EnsureBucket создаёт бакет, если его ещё нет.
//
// Метод идемпотентен: повторный вызов на существующем бакете не является ошибкой.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("s3: check bucket %q: %w", c.bucket, err)
	}
	if exists {
		c.log.Debug("s3 bucket already exists", "bucket", c.bucket)
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

	c.log.Info("s3 bucket created", "bucket", c.bucket)
	return nil
}

// Upload загружает объект по ключу.
//
// size должен быть точным размером данных: потоковая загрузка неизвестной длины
// (size = -1) не поддерживается. Если contentType пустой, применяется
// "application/octet-stream".
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if size < 0 {
		return fmt.Errorf("s3: upload %q: exact size is required, got %d", key, size)
	}
	if contentType == "" {
		contentType = defaultContentType
	}

	_, err := c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		c.log.Debug("s3 upload failed", "key", key, "error", err)
		return fmt.Errorf("s3: upload %q: %w", key, err)
	}
	return nil
}

// Download возвращает поток чтения и метаданные объекта.
//
// Если объекта нет — domain.ErrNotFound. Вызывающая сторона обязана закрыть
// возвращённый io.ReadCloser.
func (c *Client) Download(ctx context.Context, key string) (io.ReadCloser, *domain.ObjectInfo, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("s3: download %q: %w", key, err)
	}

	// GetObject ленив: ошибка отсутствующего объекта всплывает только при Stat.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("s3: download %q: %w", key, err)
	}

	return obj, toObjectInfo(info), nil
}

// StatObject возвращает метаданные объекта. Если объекта нет — domain.ErrNotFound.
func (c *Client) StatObject(ctx context.Context, key string) (*domain.ObjectInfo, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("s3: stat %q: %w", key, err)
	}
	return toObjectInfo(info), nil
}

// Delete удаляет объект.
//
// Отсутствие объекта не является ошибкой: S3 отвечает успехом на удаление
// несуществующего ключа, поэтому метод идемпотентен.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// DeleteMany удаляет несколько объектов одним batch-запросом.
//
// Ошибки отдельных ключей логируются и объединяются в одну ошибку, но не
// прерывают batch: так за один вызов удаляется максимум объектов.
func (c *Client) DeleteMany(ctx context.Context, keys []string) error {
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

	var errs []error
	for res := range c.mc.RemoveObjects(ctx, c.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		c.log.Warn("s3 batch delete failed", "key", res.ObjectName, "error", res.Err)
		errs = append(errs, fmt.Errorf("delete %q: %w", res.ObjectName, res.Err))
	}
	return errors.Join(errs...)
}

// HealthCheck проверяет доступность хранилища и наличие бакета.
// Используется в /health (ШАГ 8).
func (c *Client) HealthCheck(ctx context.Context) error {
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
