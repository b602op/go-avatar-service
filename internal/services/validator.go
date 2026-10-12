package services

import (
	"fmt"

	"github.com/b602op/avatars-service/internal/domain"
)

// Ограничения загружаемых изображений.
const (
	// MaxFileSize — максимальный размер файла аватарки (10 МиБ).
	MaxFileSize = 10 * 1024 * 1024
)

// Поддерживаемые MIME-типы.
const (
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
	mimeWebP = "image/webp"
)

// AllowedMIMEs — множество допустимых MIME-типов загружаемых файлов.
var AllowedMIMEs = map[string]struct{}{
	mimeJPEG: {},
	mimePNG:  {},
	mimeWebP: {},
}

// ValidateImage проверяет размер, MIME-тип и сигнатуру (magic bytes) файла.
//
// Возвращает domain.ErrFileTooLarge при превышении размера и
// domain.ErrInvalidFormat при пустых данных, недопустимом MIME-типе или
// несовпадении сигнатуры с заявленным типом.
func ValidateImage(data []byte, contentType string) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty data", domain.ErrInvalidFormat)
	}
	if int64(len(data)) > MaxFileSize {
		return fmt.Errorf("%w: %d bytes exceeds limit %d", domain.ErrFileTooLarge, len(data), MaxFileSize)
	}
	if _, ok := AllowedMIMEs[contentType]; !ok {
		return fmt.Errorf("%w: unsupported content type %q", domain.ErrInvalidFormat, contentType)
	}
	if !hasValidMagicBytes(data, contentType) {
		return fmt.Errorf("%w: content type %q does not match file signature", domain.ErrInvalidFormat, contentType)
	}
	return nil
}

// hasValidMagicBytes сверяет сигнатуру файла с заявленным MIME-типом.
//
// JPEG: FF D8 FF; PNG: 89 50 4E 47; WebP: "RIFF"...."WEBP".
func hasValidMagicBytes(data []byte, contentType string) bool {
	switch contentType {
	case mimeJPEG:
		return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
	case mimePNG:
		return len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47
	case mimeWebP:
		return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	default:
		return false
	}
}
