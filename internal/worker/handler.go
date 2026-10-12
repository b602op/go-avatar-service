package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/pkg/resizer"
)

// thumbnailsContentType — MIME загружаемых миниатюр (ресайзер отдаёт JPEG).
const thumbnailsContentType = "image/jpeg"

// Resizer — интерфейс изменения размера изображений.
//
// Объявлен на стороне потребителя, чтобы подменять ресайз в тестах.
type Resizer interface {
	ResizeBatch(data []byte, sizes map[string]resizer.Size) (map[string][]byte, error)
}

// ResizerFunc — функциональный адаптер над функциями pkg/resizer.
type ResizerFunc func(data []byte, sizes map[string]resizer.Size) (map[string][]byte, error)

// ResizeBatch вызывает обёрнутую функцию.
func (f ResizerFunc) ResizeBatch(data []byte, sizes map[string]resizer.Size) (map[string][]byte, error) {
	return f(data, sizes)
}

// Проверка на этапе компиляции, что адаптер реализует интерфейс.
var _ Resizer = ResizerFunc(nil)

// Handler обрабатывает события из очередей RabbitMQ.
type Handler struct {
	repo    domain.AvatarRepository
	storage domain.ObjectStorage
	resizer Resizer
	log     *slog.Logger
}

// NewHandler создаёт обработчик событий.
func NewHandler(repo domain.AvatarRepository, storage domain.ObjectStorage, rz Resizer, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{repo: repo, storage: storage, resizer: rz, log: log}
}

// HandleUpload создаёт миниатюры для загруженной аватарки и сохраняет их в S3.
//
// Ошибки S3/БД/ресайза — transient: consumer переотправит сообщение в очередь
// и в итоге отправит в DLQ (ШАГ 7.4). Невалидный ID даёт publisher.PermanentError.
// «Мягкие» случаи (аватар удалён, уже обработан) возвращают nil — сообщение ack.
func (h *Handler) HandleUpload(ctx context.Context, event publisher.AvatarUploadEvent) error {
	avatarID, err := uuid.Parse(event.AvatarID)
	if err != nil {
		// Невалидный ID не станет валидным при повторе — сразу в DLQ.
		return &publisher.PermanentError{Err: fmt.Errorf("worker: parse avatar_id %q: %w", event.AvatarID, err)}
	}

	avatar, err := h.repo.GetByID(ctx, avatarID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Аватар уже удалён — обрабатывать нечего.
			h.log.Warn("avatar not found (already deleted?)", "avatar_id", avatarID)
			return nil
		}
		return fmt.Errorf("worker: get avatar: %w", err)
	}

	// Идемпотентность: повторная доставка того же события не должна ничего делать.
	if avatar.ProcessingStatus == domain.ProcessingStatusCompleted {
		h.log.Debug("already processed, skipping", "avatar_id", avatarID)
		return nil
	}

	reader, _, err := h.storage.Download(ctx, event.S3Key)
	if err != nil {
		return fmt.Errorf("worker: download original %q: %w", event.S3Key, err)
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("worker: read original %q: %w", event.S3Key, err)
	}

	thumbs, err := h.resizer.ResizeBatch(data, resizer.DefaultSizes)
	if err != nil {
		return fmt.Errorf("worker: resize: %w", err)
	}

	thumbnails := make(map[string]string, len(thumbs))
	for size, thumbData := range thumbs {
		key := services.ThumbnailKey(avatar.UserID, avatarID, size)
		if err := h.storage.Upload(ctx, key, bytes.NewReader(thumbData), int64(len(thumbData)), thumbnailsContentType); err != nil {
			return fmt.Errorf("worker: upload thumbnail %q: %w", size, err)
		}
		thumbnails[size] = key
	}

	if err := h.repo.UpdateProcessingStatus(ctx, avatarID, domain.ProcessingStatusCompleted, thumbnails); err != nil {
		return fmt.Errorf("worker: update processing status: %w", err)
	}

	h.log.Info("avatar processed", "avatar_id", avatarID, "thumbnails", len(thumbnails))
	return nil
}

// HandleDelete удаляет файлы аватарки из S3.
//
// В БД не заглядываем: запись уже помечена удалённой до публикации события.
// Идемпотентно — удаление отсутствующих объектов не является ошибкой.
func (h *Handler) HandleDelete(ctx context.Context, event publisher.AvatarDeleteEvent) error {
	avatarID, err := uuid.Parse(event.AvatarID)
	if err != nil {
		return &publisher.PermanentError{Err: fmt.Errorf("worker: parse avatar_id %q: %w", event.AvatarID, err)}
	}

	if len(event.S3Keys) == 0 {
		h.log.Debug("no keys to delete, skipping", "avatar_id", avatarID)
		return nil
	}

	if err := h.storage.DeleteMany(ctx, event.S3Keys); err != nil {
		return fmt.Errorf("worker: delete avatar files: %w", err)
	}

	h.log.Info("avatar files deleted from s3", "avatar_id", avatarID, "count", len(event.S3Keys))
	return nil
}
