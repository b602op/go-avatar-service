package domain

import (
	"context"
	"io"
	"time"
)

// ObjectInfo — метаданные объекта в объектном хранилище.
//
// Отдельная структура вместо minio.ObjectInfo, чтобы пакет domain не зависел
// от конкретного S3-клиента.
type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// ObjectStorage описывает операции над объектным хранилищем (S3/MinIO).
//
// Интерфейс объявлен на стороне домена, чтобы сервисный слой мог мокать
// хранилище в тестах без реального S3. Реализация — internal/services/s3.
type ObjectStorage interface {
	// EnsureBucket создаёт бакет, если его нет. Идемпотентно.
	EnsureBucket(ctx context.Context) error

	// Upload загружает объект по ключу.
	//
	// size должен быть точным размером данных: потоковая загрузка неизвестной
	// длины (size = -1) не поддерживается. Если contentType пустой, применяется
	// "application/octet-stream".
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Download возвращает поток чтения и метаданные объекта.
	// Если объекта нет — ErrNotFound.
	Download(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)

	// StatObject возвращает метаданные объекта. Если объекта нет — ErrNotFound.
	StatObject(ctx context.Context, key string) (*ObjectInfo, error)

	// Delete удаляет объект. Отсутствие объекта не является ошибкой (идемпотентно).
	Delete(ctx context.Context, key string) error

	// DeleteMany удаляет несколько объектов одним batch-запросом.
	// Частичные ошибки отдельных ключей объединяются в одну ошибку.
	DeleteMany(ctx context.Context, keys []string) error

	// HealthCheck проверяет доступность хранилища и наличие бакета.
	// Используется в /health (ШАГ 8).
	HealthCheck(ctx context.Context) error
}
