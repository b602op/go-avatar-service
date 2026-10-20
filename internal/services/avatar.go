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
	"github.com/b602op/avatars-service/internal/metrics"
)

// sizeOriginal — значение параметра size, означающее выдачу оригинала.
const sizeOriginal = "original"

// contentTypeJPEG — MIME миниатюр: ресайзер всегда кодирует их в JPEG.
const contentTypeJPEG = "image/jpeg"

// defaultContentType применяется, когда MIME не удалось определить ни по
// запрошенному размеру, ни по данным в БД.
const defaultContentType = "application/octet-stream"

// AvatarService реализует бизнес-логику работы с аватарками.
//
// Слой полностью изолирован от HTTP и брокера: хендлеры вызывают только методы
// сервиса и не знают деталей S3, БД и валидации.
type AvatarService struct {
	repo      domain.AvatarRepository
	storage   domain.ObjectStorage
	publisher domain.EventPublisher
	log       *slog.Logger
	metrics   *metrics.Set
}

// NewAvatarService создаёт сервис аватарок.
//
// m обязателен: сервис пишет бизнес-метрики на каждой загрузке и удалении, и
// «молчаливый» fallback на глобальный реестр скрывал бы ошибку сборки
// зависимостей. nil — ошибка программиста, поэтому паника, а не тихая подмена.
func NewAvatarService(
	repo domain.AvatarRepository,
	storage domain.ObjectStorage,
	publisher domain.EventPublisher,
	log *slog.Logger,
	m *metrics.Set,
) *AvatarService {
	if m == nil {
		panic("services: NewAvatarService: metrics is nil")
	}

	if log == nil {
		log = slog.Default()
	}

	return &AvatarService{
		repo:      repo,
		storage:   storage,
		publisher: publisher,
		log:       log,
		metrics:   m,
	}
}

// Upload валидирует изображение, сохраняет оригинал в S3 и создаёт запись в БД.
//
// Порядок шагов: ValidateImage → генерация ID и ключа → storage.Upload →
// repo.Create → публикация события.
//
// Публикация события — best-effort: запись уже создана со статусом pending,
// поэтому сбой брокера не отменяет загрузку. Зависшую запись подберёт
// cleanup-воркер (outbox-lite) и переопубликует событие.
//
// Ошибка repo.Create компенсируется удалением уже загруженного оригинала,
// иначе файл остался бы в S3 навсегда (orphan).
//
// Возвращаемые значения именованы ради defer: он записывает метрики загрузки
// с меткой исхода, а исход известен только в момент выхода из функции.
func (s *AvatarService) Upload(
	ctx context.Context,
	userID, fileName, contentType string,
	data []byte,
) (avatar *domain.Avatar, err error) {
	start := time.Now()
	defer func() {
		status := metrics.StatusSuccess
		if err != nil {
			status = metrics.StatusError
		}
		s.metrics.AvatarsUploadsTotal.WithLabelValues(status).Inc()
		s.metrics.AvatarsUploadDuration.WithLabelValues(status).Observe(time.Since(start).Seconds())
	}()

	if err := ValidateImage(data, contentType); err != nil {
		return nil, err
	}

	avatarID := uuid.New()
	s3Key := OriginalKey(userID, avatarID, fileName)
	s.log.DebugContext(ctx, "avatar upload prepared",
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
	avatar = &domain.Avatar{
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
		// Компенсация: записи в БД нет, значит оригинал в S3 — orphan.
		// Удаляем его, чтобы не оставлять мусор. Если удалить не удалось,
		// файл остаётся навсегда: логируем громко (техдолг — нужен
		// отдельный sweeper для осиротевших объектов).
		if delErr := s.storage.Delete(ctx, s3Key); delErr != nil {
			s.log.ErrorContext(ctx, "upload compensation failed: orphan in object storage",
				"avatar_id", avatarID,
				"s3_key", s3Key,
				"delete_err", delErr,
			)
		}
		return nil, fmt.Errorf("services: create avatar record: %w", err)
	}

	if err := s.publisher.PublishAvatarUploaded(ctx, avatarID, userID, s3Key); err != nil {
		// Best-effort: запись создана, клиент получает 201. Событие
		// переопубликует cleanup-воркер, увидев pending-аватар.
		s.log.WarnContext(ctx, "avatar created but upload event was not published",
			"avatar_id", avatarID,
			"err", err,
		)
	}

	// Размер наблюдается только у принятого файла: отказы валидации — это про
	// avatars_uploads_total{status="error"}, а не про распределение размеров.
	s.metrics.AvatarsFileSize.Observe(float64(len(data)))

	s.log.InfoContext(ctx, "avatar uploaded", "avatar_id", avatarID, "user_id", userID)
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
// size == "" или "original" отдаёт оригинал из S3; "100x100" и "300x300" —
// готовые миниатюры, созданные воркером. Любой другой размер миниатюр
// не поддерживается (ресайза на лету нет) и даёт domain.ErrNotImplemented —
// хендлер вернёт 501.
func (s *AvatarService) GetContent(
	ctx context.Context,
	id uuid.UUID,
	size string,
) (io.ReadCloser, *domain.ObjectInfo, error) {
	avatar, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	key, err := contentKey(avatar, size)
	if err != nil {
		return nil, nil, err
	}

	s.log.InfoContext(ctx, "content requested", "avatar_id", id, "size", size)
	s.log.DebugContext(ctx, "downloading content", "avatar_id", id, "key", key)

	rc, info, err := s.storage.Download(ctx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("services: download content: %w", err)
	}

	// S3 не всегда хранит Content-Type объекта — подставляем его сами,
	// иначе хендлер отправит пустой заголовок и клиент не распознает картинку.
	if info.ContentType == "" {
		info.ContentType = contentTypeFor(avatar, size)
	}

	return rc, info, nil
}

// contentKey определяет S3-ключ для запрошенного размера.
//
// Возвращает domain.ErrNotImplemented, если миниатюры такого размера нет:
// она не создавалась воркером.
func contentKey(avatar *domain.Avatar, size string) (string, error) {
	if size == "" || size == sizeOriginal {
		return avatar.S3Key, nil
	}

	key, ok := avatar.ThumbnailS3Keys[size]
	if !ok {
		return "", fmt.Errorf("%w: size %q", domain.ErrNotImplemented, size)
	}
	return key, nil
}

// contentTypeFor подбирает MIME, когда хранилище его не вернуло.
//
// Оригинал отдаётся в исходном формате загрузки (mime_type из БД), миниатюры —
// всегда JPEG.
func contentTypeFor(avatar *domain.Avatar, size string) string {
	if size == "" || size == sizeOriginal {
		if avatar.MimeType != "" {
			return avatar.MimeType
		}
		return defaultContentType
	}
	return contentTypeJPEG
}

// Delete мягко удаляет аватарку и публикует событие на очистку файлов в S3.
//
// Удалять может только владелец: чужой аватар даёт domain.ErrForbidden.
// Очистка объектов в S3 выполняется асинхронно воркером по событию
// avatar.deleted: если S3 недоступен, consumer переотправит сообщение (retry).
// Поэтому сам метод не трогает storage — только БД и брокер.
//
// Именованный err нужен defer'у с метрикой исхода: отказ по правам, отсутствие
// записи и сбой публикации считаются ошибкой, успешное удаление — успехом.
func (s *AvatarService) Delete(ctx context.Context, id uuid.UUID, requesterID string) (err error) {
	defer func() {
		status := metrics.StatusSuccess
		if err != nil {
			status = metrics.StatusError
		}
		s.metrics.AvatarsDeletesTotal.WithLabelValues(status).Inc()
	}()

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
	if err := s.publisher.PublishAvatarDeleted(ctx, id, avatar.UserID, keys); err != nil {
		return fmt.Errorf("services: publish avatar deleted: %w", err)
	}

	s.log.InfoContext(ctx, "avatar deleted", "avatar_id", id, "user_id", requesterID)
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
