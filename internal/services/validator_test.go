package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
)

// Тестовые данные с корректными сигнатурами.
var (
	jpegData = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	pngData  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	webpData = []byte{'R', 'I', 'F', 'F', 0x00, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'}
)

func TestValidateImage_JPEG_Valid(t *testing.T) {
	assert.NoError(t, ValidateImage(jpegData, mimeJPEG))
}

func TestValidateImage_PNG_Valid(t *testing.T) {
	assert.NoError(t, ValidateImage(pngData, mimePNG))
}

func TestValidateImage_WebP_Valid(t *testing.T) {
	assert.NoError(t, ValidateImage(webpData, mimeWebP))
}

func TestValidateImage_TooLarge(t *testing.T) {
	data := make([]byte, MaxFileSize+1)
	copy(data, jpegData)

	err := ValidateImage(data, mimeJPEG)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrFileTooLarge)
}

func TestValidateImage_InvalidMIME(t *testing.T) {
	err := ValidateImage(jpegData, "text/plain")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}

func TestValidateImage_MagicBytesMismatch(t *testing.T) {
	// Заявлен JPEG, а данные — PNG.
	err := ValidateImage(pngData, mimeJPEG)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}

func TestValidateImage_EmptyData(t *testing.T) {
	err := ValidateImage(nil, mimeJPEG)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}
