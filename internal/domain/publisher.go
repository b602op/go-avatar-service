package domain

import (
	"context"

	"github.com/google/uuid"
)

// EventPublisher публикует события о жизненном цикле аватарок в брокер сообщений.
//
// Интерфейс объявлен сразу, чтобы сервисный слой не зависел от конкретной
// реализации: до ШАГА 7 используется no-op, затем — реальный RabbitMQ-паблишер.
type EventPublisher interface {
	// PublishAvatarUploaded уведомляет о загрузке оригинала аватарки.
	PublishAvatarUploaded(ctx context.Context, avatarID uuid.UUID, userID string, s3Key string) error

	// PublishAvatarDeleted уведомляет об удалении аватарки со списком ключей
	// (оригинал + миниатюры) для очистки в S3 воркером.
	PublishAvatarDeleted(ctx context.Context, avatarID uuid.UUID, userID string, s3Keys []string) error
}
