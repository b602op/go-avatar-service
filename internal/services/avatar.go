package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/b602op/avatars-service/internal/domain"
)

// sizeOriginal — значение параметра size, означающее выдачу оригинала.
const sizeOriginal = "original"

// AvatarService реализует бизнес-логику работы с аватарками.
//
// Слой полностью изолирован от HTTP и брокера: хендлеры вызывают только методы
// сервиса и не знают деталей S3, БД и валидации.
type AvatarService struct {
	repo      domain.AvatarRepository
	storage   domain.ObjectStorage
	publisher domain.EventPublisher
	log       *slog.Logger
}

// NewAvatarService создаёт сервис аватарок.
func NewAvatarService(
	repo domain.AvatarRepository,
	storage domain.ObjectStorage,
	publisher domain.EventPublisher,
	log *slog.Logger,
) *AvatarService {
	if log == nil {
		log = slog.Default()
	}
	return &AvatarService{
		repo:      repo,
		storage:   storage,
		publisher: publisher,
		log:       log,
	}
}

// Upload валидирует изображение, сохраняет оригинал в S3 и создаёт запись в БД.
//
// Порядок шагов: ValidateImage → генерация ID и ключа → storage.Upload →
// repo.Create → публикация события. Ошибки не логируются, а возвращаются наверх.
func (s *AvatarService) Upload(
	ctx context.Context,
	userID, fileName, contentType string,
	data []byte,
) (*domain.Avatar, error) {
	if err := ValidateImage(data, contentType); err != nil {
		return nil, err
	}

	avatarID := uuid.New()
	s3Key := OriginalKey(userID, avatarID, fileName)
	s.log.Debug("avatar upload prepared",
		"avatar_id", avatarID,
		"user_id", userID,
		"key", s3Key,
		"size", len(data),
		"content_type", contentType,
	)

	if err := s.storage.Upload(ctx, s3Key, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
		return nil, fmt.Errorf("services: upload original: %w", err)
	}

	now := time.Now().UTC()
	avatar := &domain.Avatar{
		ID:               avatarID,
		UserID:           userID,
		FileName:         fileName,
		MimeType:         contentType,
		SizeBytes:        int64(len(data)),
		S3Key:            s3Key,
		UploadStatus:     domain.UploadStatusUploaded,
		ProcessingStatus: domain.ProcessingStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := s.repo.Create(ctx, avatar); err != nil {
		return nil, fmt.Errorf("services: create avatar record: %w", err)
	}

	if err := s.publisher.PublishAvatarUploaded(ctx, avatarID, userID, s3Key); err != nil {
		return nil, fmt.Errorf("services: publish avatar uploaded: %w", err)
	}

	s.log.Info("avatar uploaded", "avatar_id", avatarID, "user_id", userID)
	return avatar, nil
}

// GetByID возвращает аватар по идентификатору.
func (s *AvatarService) GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error) {
	return s.repo.GetByID(ctx, id)
}

// ListByUserID возвращает все активные аватары пользователя.
func (s *AvatarService) ListByUserID(ctx context.Context, userID string) ([]*domain.Avatar, error) {
	return s.repo.ListByUserID(ctx, userID)
}

// GetContent возвращает содержимое аватарки.
//
// size == "" или "original" отдаёт оригинал из S3; любой другой размер пока не
// поддерживается и даёт domain.ErrNotImplemented (хендлер вернёт 501).
func (s *AvatarService) GetContent(
	ctx context.Context,
	id uuid.UUID,
	size string,
) (io.ReadCloser, *domain.ObjectInfo, error) {
	avatar, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	s.log.Info("content requested", "avatar_id", id, "size", size)

	if size != "" && size != sizeOriginal {
		return nil, nil, fmt.Errorf("%w: size %q", domain.ErrNotImplemented, size)
	}

	s.log.Debug("downloading original", "avatar_id", id, "key", avatar.S3Key)
	rc, info, err := s.storage.Download(ctx, avatar.S3Key)
	if err != nil {
		return nil, nil, fmt.Errorf("services: download content: %w", err)
	}
	return rc, info, nil
}

// Delete мягко удаляет аватарку и очищает её файлы в S3.
//
// Удалять может только владелец: чужой аватар даёт domain.ErrForbidden.
// Сначала выполняется soft-delete в БД, затем удаление объектов в S3: если S3
// недоступен, запись в БД уже помечена удалённой, но ошибка возвращается наверх
// (на ШАГЕ 7 очистку S3 перенесём в асинхронный воркер с retry).
func (s *AvatarService) Delete(ctx context.Context, id uuid.UUID, requesterID string) error {
	avatar, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}

	if avatar.UserID != requesterID {
		return fmt.Errorf("%w: avatar %q belongs to another user", domain.ErrForbidden, id)
	}

	if err := s.repo.SoftDelete(ctx, id); err != nil {
		return fmt.Errorf("services: soft delete avatar: %w", err)
	}

	keys := collectS3Keys(avatar)
	if err := s.storage.DeleteMany(ctx, keys); err != nil {
		s.log.Warn("failed to delete avatar objects", "avatar_id", id, "error", err)
		return fmt.Errorf("services: delete avatar objects: %w", err)
	}

	if err := s.publisher.PublishAvatarDeleted(ctx, id, avatar.UserID, keys); err != nil {
		return fmt.Errorf("services: publish avatar deleted: %w", err)
	}

	s.log.Info("avatar deleted", "avatar_id", id, "user_id", requesterID)
	return nil
}

// collectS3Keys собирает ключи оригинала и миниатюр для очистки в S3.
//
// Миниатюры сортируются для детерминированного порядка.
func collectS3Keys(avatar *domain.Avatar) []string {
	keys := make([]string, 0, 1+len(avatar.ThumbnailS3Keys))
	keys = append(keys, avatar.S3Key)

	thumbs := make([]string, 0, len(avatar.ThumbnailS3Keys))
	for _, key := range avatar.ThumbnailS3Keys {
		thumbs = append(thumbs, key)
	}
	sort.Strings(thumbs)

	return append(keys, thumbs...)
}
