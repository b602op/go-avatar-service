package services

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestOriginalKey(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	got := OriginalKey("user-1", id, "photo.jpg")
	want := "originals/user-1/11111111-2222-3333-4444-555555555555/photo.jpg"
	assert.Equal(t, want, got)
}

func TestThumbnailKey(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	got := ThumbnailKey("user-1", id, "100x100")
	want := "thumbnails/user-1/11111111-2222-3333-4444-555555555555/100x100.jpg"
	assert.Equal(t, want, got)
}
