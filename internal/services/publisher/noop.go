package publisher

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/b602op/avatars-service/internal/domain"
)

// Проверка на этапе компиляции, что NoopPublisher реализует доменный интерфейс.
var _ domain.EventPublisher = (*NoopPublisher)(nil)

// NoopPublisher — заглушка EventPublisher, не публикующая события.
//
// Используется до ШАГА 7: позволяет собрать и протестировать сервисный слой без
// RabbitMQ. На ШАГЕ 7 заменяется на реальную реализацию.
type NoopPublisher struct {
	log *slog.Logger
}

// NewNoop создаёт no-op паблишер.
func NewNoop(log *slog.Logger) *NoopPublisher {
	if log == nil {
		log = slog.Default()
	}
	return &NoopPublisher{log: log}
}

// PublishAvatarUploaded логирует пропуск публикации и возвращает nil.
func (p *NoopPublisher) PublishAvatarUploaded(_ context.Context, avatarID uuid.UUID, userID, s3Key string) error {
	p.log.Debug("publish skipped (no-op)",
		"event", "avatar.uploaded",
		"avatar_id", avatarID,
		"user_id", userID,
		"key", s3Key,
	)
	return nil
}

// PublishAvatarDeleted логирует пропуск публикации и возвращает nil.
func (p *NoopPublisher) PublishAvatarDeleted(_ context.Context, avatarID uuid.UUID, userID string, s3Keys []string) error {
	p.log.Debug("publish skipped (no-op)",
		"event", "avatar.deleted",
		"avatar_id", avatarID,
		"user_id", userID,
		"keys", len(s3Keys),
	)
	return nil
}
