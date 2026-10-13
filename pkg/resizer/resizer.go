package resizer

import (
	"bytes"
	"fmt"
	"image"

	"github.com/disintegration/imaging"

	// Регистрирует WebP-декодер в image.Decode: imaging сам его не подключает,
	// а валидатор сервиса допускает image/webp на вход.
	_ "golang.org/x/image/webp"
)

// jpegQuality — качество выходного JPEG.
const jpegQuality = 85

// Size — размеры миниатюры в пикселях.
type Size struct {
	Width  int
	Height int
}

// DefaultSizes — стандартные размеры миниатюр (из ТЗ).
var DefaultSizes = map[string]Size{
	"100x100": {Width: 100, Height: 100},
	"300x300": {Width: 300, Height: 300},
}

// GetDimensions читает ширину и высоту изображения из его заголовка.
//
// Использует image.DecodeConfig: декодируется только header, а не всё
// изображение, поэтому вызов дешёвый. Поддерживаемые форматы те же, что и у
// Resize (JPEG, PNG, GIF, WebP).
func GetDimensions(data []byte) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("resizer: decode config: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// Resize уменьшает изображение до рамки Width x Height и кодирует его в JPEG.
//
// Использует imaging.Fit: пропорции сохраняются, изображение вписывается в
// рамку (по большей стороне). Изображения меньше рамки не увеличиваются.
func Resize(data []byte, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("resizer: invalid size %dx%d", width, height)
	}

	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("resizer: decode: %w", err)
	}

	resized := imaging.Fit(img, width, height, imaging.Lanczos)

	var buf bytes.Buffer
	if err := imaging.Encode(&buf, resized, imaging.JPEG, imaging.JPEGQuality(jpegQuality)); err != nil {
		return nil, fmt.Errorf("resizer: encode: %w", err)
	}

	return buf.Bytes(), nil
}

// ResizeBatch создаёт несколько миниатюр за один проход декодирования.
//
// Возвращает map[имя размера] → JPEG-байты. Пустой список размеров — ошибка.
func ResizeBatch(data []byte, sizes map[string]Size) (map[string][]byte, error) {
	if len(sizes) == 0 {
		return nil, fmt.Errorf("resizer: empty sizes")
	}
	for name, s := range sizes {
		if s.Width <= 0 || s.Height <= 0 {
			return nil, fmt.Errorf("resizer: invalid size for %q: %dx%d", name, s.Width, s.Height)
		}
	}

	// Декодируем оригинал один раз — это самая дорогая операция.
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("resizer: decode: %w", err)
	}

	result := make(map[string][]byte, len(sizes))
	for name, s := range sizes {
		resized := imaging.Fit(img, s.Width, s.Height, imaging.Lanczos)

		var buf bytes.Buffer
		if err := imaging.Encode(&buf, resized, imaging.JPEG, imaging.JPEGQuality(jpegQuality)); err != nil {
			return nil, fmt.Errorf("resizer: encode %s: %w", name, err)
		}
		result[name] = buf.Bytes()
	}

	return result, nil
}
