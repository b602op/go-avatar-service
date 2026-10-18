package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/httpx"
	"github.com/b602op/avatars-service/internal/metrics"
	"github.com/b602op/avatars-service/internal/services"
)

// maxMultipartMemory — лимит памяти на разбор multipart-формы.
const maxMultipartMemory = 10 << 20

// genericContentType — «неизвестный» MIME, который шлют curl -F и multipart.Writer.
const genericContentType = "application/octet-stream"

// sizeOriginal — значение параметра size, означающее выдачу оригинала.
const sizeOriginal = "original"

// contentCacheControl — заголовок кэширования для байтов аватарки.
//
// Содержимое неизменяемо (у аватарки свой ID), поэтому кэшируем на сутки.
const contentCacheControl = "public, max-age=86400"

// AvatarService описывает бизнес-операции над аватарками, нужные хендлерам.
//
// Интерфейс объявлен на стороне потребителя (идиоматичный Go): это позволяет
// подменять сервис в тестах без реальных БД/S3. Реализация — *services.AvatarService.
type AvatarService interface {
	Upload(ctx context.Context, userID, fileName, contentType string, data []byte) (*domain.Avatar, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error)
	ListByUserID(ctx context.Context, userID string) ([]*domain.Avatar, error)
	GetContent(ctx context.Context, id uuid.UUID, size string) (io.ReadCloser, *domain.ObjectInfo, error)
	Delete(ctx context.Context, id uuid.UUID, requesterID string) error
}

// Проверка на этапе компиляции, что конкретный сервис реализует интерфейс.
var _ AvatarService = (*services.AvatarService)(nil)

// AvatarHandler — HTTP-обработчики REST API аватарок.
type AvatarHandler struct {
	svc AvatarService
	log *slog.Logger
}

// NewAvatarHandler создаёт хендлер аватарок.
func NewAvatarHandler(svc AvatarService, log *slog.Logger) *AvatarHandler {
	if log == nil {
		log = slog.Default()
	}
	return &AvatarHandler{svc: svc, log: log}
}

// Upload обрабатывает POST /api/v1/avatars (multipart/form-data, поле "image").
func (h *AvatarHandler) Upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := httpx.UserIDFromContext(ctx)
	if !ok || userID == "" {
		h.log.WarnContext(ctx, "user id missing in context")
		respondJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "Unauthorized"})
		return
	}

	// Ограничиваем тело до чтения, чтобы не выжрать память на гигантском файле.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+1024)
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondJSON(w, http.StatusRequestEntityTooLarge, FileTooLargeResponse{
				Error:   "File too large",
				MaxSize: maxUploadSize,
			})
			return
		}
		h.log.WarnContext(ctx, "invalid multipart form", "error", err)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid multipart form"})
		return
	}

	// Поле называется "image" — контракт фронтенда (web/static/index.html).
	file, header, err := r.FormFile("image")
	if err != nil {
		h.log.WarnContext(ctx, "missing file field", "error", err)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{
			Error:   "Missing file",
			Details: "Field 'image' is required",
		})
		return
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		respondError(ctx, w, h.log, fmt.Errorf("handlers: read uploaded file: %w", err))
		return
	}
	if int64(len(data)) > services.MaxFileSize {
		respondJSON(w, http.StatusRequestEntityTooLarge, FileTooLargeResponse{
			Error:   "File too large",
			MaxSize: maxUploadSize,
		})
		return
	}

	// Определяем MIME: пустой заголовок ИЛИ generic octet-stream (так шлют curl -F
	// и multipart.Writer) означают «тип неизвестен» — определяем по сигнатуре файла.
	// Валидность всё равно проверит сервис по magic bytes.
	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == genericContentType {
		contentType = http.DetectContentType(data)
	}

	avatar, err := h.svc.Upload(ctx, userID, header.Filename, contentType, data)
	if err != nil {
		respondError(ctx, w, h.log, err)
		return
	}

	// size_class — категория размера для человекочитаемых выборок в логах;
	// в метрики она не идёт (там бакеты гистограммы avatars_file_size_bytes).
	h.log.InfoContext(ctx, "avatar uploaded",
		"avatar_id", avatar.ID,
		"user_id", userID,
		"size_bytes", len(data),
		"size_class", metrics.SizeClass(int64(len(data))),
	)
	respondJSON(w, http.StatusCreated, UploadResponse{
		ID:        avatar.ID.String(),
		UserID:    avatar.UserID,
		URL:       avatarURL(avatar.ID.String()),
		Status:    "processing",
		CreatedAt: avatar.CreatedAt,
	})
}

// GetByID обрабатывает GET /api/v1/avatars/{avatar_id}.
func (h *AvatarHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseAvatarID(w, r)
	if !ok {
		return
	}

	avatar, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		respondError(r.Context(), w, h.log, err)
		return
	}

	h.log.InfoContext(r.Context(), "avatar fetched", "avatar_id", id)
	respondJSON(w, http.StatusOK, avatarToResponse(avatar))
}

// GetUserAvatar обрабатывает GET /api/v1/users/{user_id}/avatar.
//
// Отдаёт самый свежий аватар пользователя (репозиторий сортирует по created_at DESC).
func (h *AvatarHandler) GetUserAvatar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := chi.URLParam(r, "user_id")

	avatars, err := h.svc.ListByUserID(ctx, userID)
	if err != nil {
		respondError(ctx, w, h.log, err)
		return
	}
	if len(avatars) == 0 {
		respondError(ctx, w, h.log, domain.ErrNotFound)
		return
	}

	h.log.InfoContext(ctx, "user avatar fetched", "user_id", userID, "avatar_id", avatars[0].ID)
	respondJSON(w, http.StatusOK, avatarToResponse(avatars[0]))
}

// ListByUser обрабатывает GET /api/v1/users/{user_id}/avatars.
func (h *AvatarHandler) ListByUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := chi.URLParam(r, "user_id")

	avatars, err := h.svc.ListByUserID(ctx, userID)
	if err != nil {
		respondError(ctx, w, h.log, err)
		return
	}

	resp := make([]AvatarResponse, 0, len(avatars))
	for _, a := range avatars {
		resp = append(resp, avatarToResponse(a))
	}

	h.log.InfoContext(ctx, "avatars listed", "user_id", userID, "count", len(resp))
	respondJSON(w, http.StatusOK, resp)
}

// Metadata обрабатывает GET /api/v1/avatars/{avatar_id}/metadata.
func (h *AvatarHandler) Metadata(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseAvatarID(w, r)
	if !ok {
		return
	}

	avatar, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		respondError(r.Context(), w, h.log, err)
		return
	}

	h.log.InfoContext(r.Context(), "avatar metadata fetched", "avatar_id", id)
	respondJSON(w, http.StatusOK, avatarToMetadata(avatar))
}

// GetContent обрабатывает GET /api/v1/avatars/{avatar_id}/content.
//
// Отдаёт байты аватарки: оригинал (size пусто или "original") либо готовую
// миниатюру (size=100x100, size=300x300). Ресайза на лету нет — неизвестный
// размер даёт 501 (domain.ErrNotImplemented).
func (h *AvatarHandler) GetContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := h.parseAvatarID(w, r)
	if !ok {
		return
	}

	size := r.URL.Query().Get("size")
	if size == "" {
		size = sizeOriginal
	}

	reader, info, err := h.svc.GetContent(ctx, id, size)
	if err != nil {
		respondError(ctx, w, h.log, err)
		return
	}
	defer func() { _ = reader.Close() }()

	h.log.InfoContext(ctx, "avatar content served", "avatar_id", id, "size", size, "bytes", info.Size)

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Cache-Control", contentCacheControl)
	if info.ETag != "" {
		w.Header().Set("ETag", `"`+info.ETag+`"`)
	}
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		// Ответ уже начал уходить: клиент мог отключиться или получить обрыв.
		// Это не 5xx приложения — фиксируем в логе и завершаем обработку.
		h.log.WarnContext(ctx, "avatar content copy failed", "avatar_id", id, "size", size, "error", err)
	}
}

// Delete обрабатывает DELETE /api/v1/avatars/{avatar_id}.
func (h *AvatarHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseAvatarID(w, r)
	if !ok {
		return
	}
	h.deleteAvatar(w, r, id)
}

// DeleteUserAvatar обрабатывает DELETE /api/v1/users/{user_id}/avatar.
//
// Удаляет самый свежий аватар пользователя.
func (h *AvatarHandler) DeleteUserAvatar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := chi.URLParam(r, "user_id")

	avatars, err := h.svc.ListByUserID(ctx, userID)
	if err != nil {
		respondError(ctx, w, h.log, err)
		return
	}
	if len(avatars) == 0 {
		respondError(ctx, w, h.log, domain.ErrNotFound)
		return
	}

	h.deleteAvatar(w, r, avatars[0].ID)
}

// deleteAvatar — общая логика удаления: проверка владельца и ответ 204.
func (h *AvatarHandler) deleteAvatar(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()

	requesterID, ok := httpx.UserIDFromContext(ctx)
	if !ok || requesterID == "" {
		h.log.WarnContext(ctx, "user id missing in context")
		respondJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "Unauthorized"})
		return
	}

	if err := h.svc.Delete(ctx, id, requesterID); err != nil {
		respondError(ctx, w, h.log, err)
		return
	}

	h.log.InfoContext(ctx, "avatar deleted", "avatar_id", id, "user_id", requesterID)
	w.WriteHeader(http.StatusNoContent)
}

// parseAvatarID читает и парсит path-параметр avatar_id.
// При невалидном UUID отвечает 400 и возвращает ok=false.
func (h *AvatarHandler) parseAvatarID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "avatar_id")
	id, err := uuid.Parse(raw)
	if err != nil {
		h.log.WarnContext(r.Context(), "invalid avatar id", "raw", raw, "error", err)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid avatar ID"})
		return uuid.Nil, false
	}
	return id, true
}
