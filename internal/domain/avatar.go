package domain

import (
	"time"

	"github.com/google/uuid"
)

// UploadStatus — статус загрузки файла в S3.
type UploadStatus string

const (
	UploadStatusUploading UploadStatus = "uploading" // файл загружается в S3
	UploadStatusUploaded  UploadStatus = "uploaded"  // файл успешно загружен
	UploadStatusFailed    UploadStatus = "failed"    // ошибка загрузки
)

// ProcessingStatus — статус асинхронной обработки (миниатюры и т.п.).
type ProcessingStatus string

const (
	ProcessingStatusPending    ProcessingStatus = "pending"    // ждёт обработки
	ProcessingStatusProcessing ProcessingStatus = "processing" // обрабатывается воркером
	ProcessingStatusCompleted  ProcessingStatus = "completed"  // обработка завершена
	ProcessingStatusFailed     ProcessingStatus = "failed"     // ошибка обработки
)

// Avatar — доменная модель аватарки пользователя.
type Avatar struct {
	ID               uuid.UUID
	UserID           string
	FileName         string
	MimeType         string
	SizeBytes        int64
	S3Key            string            // ключ оригинала в S3
	ThumbnailS3Keys  map[string]string // {"100x100": "thumbnails/...", "300x300": "..."}
	UploadStatus     UploadStatus
	ProcessingStatus ProcessingStatus
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time // nil = активный, не nil = мягко удалён
}

// IsDeleted возвращает true, если аватар мягко удалён.
func (a *Avatar) IsDeleted() bool {
	return a.DeletedAt != nil
}
