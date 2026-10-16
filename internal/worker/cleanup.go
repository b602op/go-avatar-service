package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/b602op/avatars-service/internal/domain"
)

// Cleanup — outbox-lite: периодически перепубликует события для «зависших»
// pending-аватарок, события которых могли потеряться (например, сервер упал
// между записью в БД и публикацией в брокер).
//
// Это не полноценный outbox (нет отдельной таблицы и транзакции), но закрывает
// основной сценарий потери события с минимальной ценой.
type Cleanup struct {
	repo      domain.AvatarRepository
	publisher domain.EventPublisher
	log       *slog.Logger
	interval  time.Duration
	threshold time.Duration
}

// NewCleanup создаёт фоновый подметальщик.
//
// interval — период проверки, threshold — минимальный «возраст» pending-записи,
// после которого она считается зависшей.
func NewCleanup(
	repo domain.AvatarRepository,
	pub domain.EventPublisher,
	log *slog.Logger,
	interval, threshold time.Duration,
) *Cleanup {
	if log == nil {
		log = slog.Default()
	}
	// Защита от паники time.NewTicker при некорректном интервале.
	if interval <= 0 {
		interval = time.Minute
	}
	return &Cleanup{
		repo:      repo,
		publisher: pub,
		log:       log,
		interval:  interval,
		threshold: threshold,
	}
}

// Run запускает периодическую проверку до отмены контекста.
func (c *Cleanup) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce перепубликует события для pending-аватарок старше threshold.
func (c *Cleanup) runOnce(ctx context.Context) {
	avatars, err := c.repo.ListPending(ctx, c.threshold)
	if err != nil {
		c.log.Error("cleanup: list pending failed", "error", err)
		return
	}
	if len(avatars) == 0 {
		return
	}

	c.log.Info("cleanup: found pending", "count", len(avatars))
	for _, a := range avatars {
		c.republish(ctx, a)
	}
	c.log.Info("cleanup: done")
}

// republish помечает запись processing и перепубликует событие загрузки.
//
// Метка снимает запись с выборки ListPending (та ищет только pending): без неё
// «отравленное» событие переопубликовывалось бы каждую итерацию бесконечно.
// Дальше запись доведёт до completed воркер, а при transient-ошибке — retry-очередь.
//
// Если публикация не удалась, статус откатывается в pending, чтобы попробовать
// на следующем круге.
func (c *Cleanup) republish(ctx context.Context, a *domain.Avatar) {
	if err := c.repo.UpdateProcessingStatus(ctx, a.ID, domain.ProcessingStatusProcessing, nil); err != nil {
		c.log.Error("cleanup: mark processing failed", "avatar_id", a.ID, "error", err)
		return
	}

	if err := c.publisher.PublishAvatarUploaded(ctx, a.ID, a.UserID, a.S3Key); err != nil {
		c.log.Error("cleanup: republish failed", "avatar_id", a.ID, "error", err)
		if rbErr := c.repo.UpdateProcessingStatus(ctx, a.ID, domain.ProcessingStatusPending, nil); rbErr != nil {
			c.log.Error("cleanup: rollback to pending failed", "avatar_id", a.ID, "error", rbErr)
		}
		return
	}

	c.log.Info("cleanup: republished", "avatar_id", a.ID)
}
