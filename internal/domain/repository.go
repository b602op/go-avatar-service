package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AvatarRepository описывает операции хранения метаданных аватарок.
type AvatarRepository interface {
	// Create создаёт запись в БД. ID должен быть заполнен вызывающей стороной.
	Create(ctx context.Context, avatar *Avatar) error

	// GetByID возвращает аватар по ID. Если запись не найдена или мягко удалена — ErrNotFound.
	GetByID(ctx context.Context, id uuid.UUID) (*Avatar, error)

	// ListByUserID возвращает все активные (не удалённые) аватары пользователя.
	ListByUserID(ctx context.Context, userID string) ([]*Avatar, error)

	// UpdateProcessingStatus обновляет статус обработки и ключи миниатюр.
	UpdateProcessingStatus(ctx context.Context, id uuid.UUID, status ProcessingStatus, thumbnailKeys map[string]string) error

	// CompleteProcessing фиксирует успешное завершение обработки: статус
	// completed, ключи миниатюр и размеры оригинала одним запросом.
	// Возвращает ErrNotFound, если записи нет или она мягко удалена.
	CompleteProcessing(ctx context.Context, id uuid.UUID, thumbnails map[string]string, width, height int) error

	// UpdateUploadStatus обновляет только upload_status.
	UpdateUploadStatus(ctx context.Context, id uuid.UUID, status UploadStatus) error

	// SoftDelete помечает deleted_at = NOW(). Возвращает ErrNotFound, если записи нет.
	SoftDelete(ctx context.Context, id uuid.UUID) error

	// ListPending возвращает аватары в статусе pending, созданные раньше olderThan.
	// Используется cleanup-воркером для подметания зависших записей.
	ListPending(ctx context.Context, olderThan time.Duration) ([]*Avatar, error)
}
