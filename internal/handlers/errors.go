package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/pkg/resizer"
)

// contentTypeJSON — единый Content-Type для всех JSON-ответов.
const contentTypeJSON = "application/json; charset=utf-8"

// maxUploadSize — максимальный размер загружаемого файла (единый источник — сервис).
const maxUploadSize = services.MaxFileSize

// Проверки на этапе компиляции сигнатур хелперов.
var (
	_ func(http.ResponseWriter, int, any)            = respondJSON
	_ func(http.ResponseWriter, *slog.Logger, error) = respondError
)

// respondJSON пишет успешный JSON-ответ с заданным статусом.
func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// respondError — централизованный маппинг доменных ошибок в HTTP-ответы.
//
// 4xx не логируем, 5xx — на уровне Error
// (серверная). Детали внутренних ошибок наружу не отдаются.
func respondError(w http.ResponseWriter, log *slog.Logger, err error) {
	status, payload := mapError(err)

	if status >= http.StatusInternalServerError {
		log.Error("request failed", "status", status, "error", err)
	}

	respondJSON(w, status, payload)
}

// mapError сопоставляет доменную ошибку HTTP-статусу и телу ответа.
func mapError(err error) (int, any) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, ErrorResponse{Error: "Avatar not found"}
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, ErrorResponse{
			Error:   "Forbidden",
			Details: "You can only delete your own avatars",
		}
	case errors.Is(err, domain.ErrInvalidFormat):
		return http.StatusBadRequest, ErrorResponse{
			Error:   "Invalid file format",
			Details: "Supported formats: jpeg, png, webp",
		}
	case errors.Is(err, domain.ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge, FileTooLargeResponse{
			Error:   "File too large",
			MaxSize: maxUploadSize,
		}
	case errors.Is(err, domain.ErrNotImplemented):
		return http.StatusNotImplemented, ErrorResponse{
			Error:   "Not Implemented",
			Details: availableSizes(),
		}
	default:
		return http.StatusInternalServerError, ErrorResponse{Error: "Internal Server Error"}
	}
}

// availableSizes перечисляет поддерживаемые значения параметра size для подсказки
// в ответе 501: "original" плюс размеры миниатюр из ресайзера.
//
// Список строится из resizer.DefaultSizes, чтобы подсказка не расходилась с
// реально создаваемыми миниатюрами.
func availableSizes() string {
	sizes := make([]string, 0, len(resizer.DefaultSizes))
	for name := range resizer.DefaultSizes {
		sizes = append(sizes, name)
	}
	sort.Strings(sizes)

	return "available sizes: original, " + strings.Join(sizes, ", ")
}
