package services

import (
	"fmt"

	"github.com/google/uuid"
)

// Префиксы ключей в объектном хранилище.
//
// Группировка по userID/{avatarID} позволяет одним batch-запросом удалить все
// файлы пользователя или конкретной аватарки (ListObjects + RemoveObjects).
const (
	prefixOriginals  = "originals"
	prefixThumbnails = "thumbnails"
)

// OriginalKey собирает ключ оригинала в S3: originals/{userID}/{avatarID}/{fileName}.
func OriginalKey(userID string, avatarID uuid.UUID, fileName string) string {
	return fmt.Sprintf("%s/%s/%s/%s", prefixOriginals, userID, avatarID, fileName)
}

// ThumbnailKey собирает ключ миниатюры в S3: thumbnails/{userID}/{avatarID}/{size}.jpg.
func ThumbnailKey(userID string, avatarID uuid.UUID, size string) string {
	return fmt.Sprintf("%s/%s/%s/%s.jpg", prefixThumbnails, userID, avatarID, size)
}
