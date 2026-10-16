package handlers

import (
	"fmt"
	"sort"
	"time"

	"github.com/b602op/avatars-service/internal/domain"
)

// AvatarResponse — публичное представление аватарки.
type AvatarResponse struct {
	ID               string          `json:"id"`
	UserID           string          `json:"user_id"`
	FileName         string          `json:"file_name"`
	MimeType         string          `json:"mime_type"`
	Size             int64           `json:"size"`
	URL              string          `json:"url"`
	Status           string          `json:"status"`            // upload_status
	ProcessingStatus string          `json:"processing_status"` // processing_status
	Thumbnails       []ThumbnailInfo `json:"thumbnails,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// ThumbnailInfo — миниатюра: размер и ссылка.
//
// На этом шаге в URL кладётся S3-ключ (миниатюры ещё не раздаются, это ШАГ 7).
type ThumbnailInfo struct {
	Size string `json:"size"`
	URL  string `json:"url"`
}

// UploadResponse — ответ на успешную загрузку (201).
type UploadResponse struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	URL       string    `json:"url"`
	Status    string    `json:"status"` // "processing"
	CreatedAt time.Time `json:"created_at"`
}

// Dimensions — размеры изображения в пикселях.
type Dimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// MetadataResponse — метаданные аватарки.
type MetadataResponse struct {
	ID               string          `json:"id"`
	UserID           string          `json:"user_id"`
	FileName         string          `json:"file_name"`
	MimeType         string          `json:"mime_type"`
	Size             int64           `json:"size"`
	ProcessingStatus string          `json:"processing_status"`
	Dimensions       *Dimensions     `json:"dimensions,omitempty"` // nil, пока оригинал не обработан
	Thumbnails       []ThumbnailInfo `json:"thumbnails"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// ErrorResponse — тело ответа об ошибке.
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// FileTooLargeResponse — тело ответа 413 с максимально допустимым размером.
type FileTooLargeResponse struct {
	Error   string `json:"error"`
	MaxSize int64  `json:"max_size"`
}

// avatarURL формирует публичный URL аватарки.
func avatarURL(id string) string {
	return fmt.Sprintf("/api/v1/avatars/%s", id)
}

// avatarToResponse преобразует доменную модель в публичное представление.
func avatarToResponse(a *domain.Avatar) AvatarResponse {
	id := a.ID.String()
	return AvatarResponse{
		ID:               id,
		UserID:           a.UserID,
		FileName:         a.FileName,
		MimeType:         a.MimeType,
		Size:             a.SizeBytes,
		URL:              avatarURL(id),
		Status:           string(a.UploadStatus),
		ProcessingStatus: string(a.ProcessingStatus),
		Thumbnails:       buildThumbnails(a.ThumbnailS3Keys),
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}

// avatarToMetadata преобразует доменную модель в ответ метаданных.
func avatarToMetadata(a *domain.Avatar) MetadataResponse {
	var dims *Dimensions
	if a.Width != nil && a.Height != nil {
		dims = &Dimensions{Width: *a.Width, Height: *a.Height}
	}

	return MetadataResponse{
		ID:               a.ID.String(),
		UserID:           a.UserID,
		FileName:         a.FileName,
		MimeType:         a.MimeType,
		Size:             a.SizeBytes,
		ProcessingStatus: string(a.ProcessingStatus),
		Dimensions:       dims, // nil → поле отсутствует в JSON
		Thumbnails:       buildThumbnails(a.ThumbnailS3Keys),
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}

// buildThumbnails превращает map миниатюр в срез, отсортированный по размеру.
//
// Сортировка обязательна: итерация по map в Go недетерминирована.
func buildThumbnails(keys map[string]string) []ThumbnailInfo {
	thumbs := make([]ThumbnailInfo, 0, len(keys))
	for size, key := range keys {
		thumbs = append(thumbs, ThumbnailInfo{Size: size, URL: key})
	}
	sort.Slice(thumbs, func(i, j int) bool { return thumbs[i].Size < thumbs[j].Size })
	return thumbs
}
