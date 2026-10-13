package resizer

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webpFixtureB64 — реальный WebP 200x150 (сгенерирован ffmpeg: libwebp).
// imaging не умеет WebP-encode, поэтому вход для теста вшит как фикстура.
const webpFixtureB64 = "UklGRmwDAABXRUJQVlA4IGADAAAwHQCdASrIAJYAP/3+/3+/vD+yIhgNs/A/iWZu3Vx1xvJCbQvP9FTcdvrdp9Ox0UiwwqkviXVVOCEYoKoMnCOJG3wInf5CPQfY5bX3+9CUjFkIMNSAPAQ6T+mkay3rSKN+LvKjxWEoAWrgdzYLTzEXKztRfV7bwfro7751h8HRRASnVHsX273o55NxE5lsYeQcaN/gfqI0OQxdkTJT/sg0FBlQClKJUBlaTkIaoXOgVTAGHtfALSOFRfgEGW5CSJt5Ocw+rcnmDMH+vbRZHrIQFyzf1y1bjPdXGDBKujvB9XV7nZb6BYCAZCpCvROHaN/HAAD+5rpcENXx997GlrTZN9NQeavFucLxGmpsQS90BZUyINNSjPVpF1xiPRtOzvDdpNDkiPO90ecr4vyTvnd2tSy75zuv/b9iJgnWwGpW9wHed7IwmQOBXSMV8AxXrZIdwBICveoZF9P94IPkW0astwHhzxAjGAl9SfdUITqCbmpRuq5E/JodcdW0xnaBrkRd7PQKPmqitGU8jqCY6G/A0mFUK7Ujwaun9m+QdQNNLL8zCtdOz6gBs4CRedr2MpyOADqMGE2S8qZsX+vI9X5AEfJMEXFXPf/SB8CYSUwD4d21rXr/uJhDsxoEcoS6OKy7OF/R4E7kAkywrkf/Gan0p3sgkq2T7QY9h6OeSah/HXjRo//Fpcl+ZKGa4bfNNfbi1qQvMynD9VEkT1/r6vtqxfEtzl+Sd5xTkSNIEMjWhxTpjQjdl53mdJ1q5SncQ+pW2Esu1ICUxFFgH8eIuuegVOwIGmDLbMo4vMewfUntTEOsKUtk147pT6cB3I4wDNfaaRb4nvdyQuoMRsDZ36W7o5GC0Qj7d2OMxRaE+ONQrc0up1oM4U9dTd+jsMzztwDQ6CYGZcAGGtCZWEJjvPzHbcUtY2rSs7FgE2FjKY8ySZeD/U5FVqrZszPpchvYBen9P2Qf7Aa+682oAz37BMKEwYuaMVONo5UrCjeDgbc0vSJr9jzSMTmFiHNT78xGpzsPMHpnl0wSS8uECE00OOqGgY6iRODN/K+gjfnVwIa3N2UmpIvgEXllEUWLgDa3iY4333lVRMr38bssq/h+3kKp6BOUYqgo+4JEB4RO1IRqHyEMa0wEpAQXSNEhV25QAAA="

// makeTestImage создаёт изображение заданного размера с градиентной заливкой.
func makeTestImage(t *testing.T, w, h int, format imaging.Format) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}

	var buf bytes.Buffer
	require.NoError(t, imaging.Encode(&buf, img, format))
	return buf.Bytes()
}

// decodeSize декодирует изображение и возвращает его ширину и высоту.
func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()

	img, err := imaging.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	bounds := img.Bounds()
	return bounds.Dx(), bounds.Dy()
}

func TestResize_JPEG_100x100(t *testing.T) {
	src := makeTestImage(t, 500, 500, imaging.JPEG)

	out, err := Resize(src, 100, 100)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	w, h := decodeSize(t, out)
	assert.Equal(t, 100, w)
	assert.Equal(t, 100, h)
}

func TestResize_PNG_300x300(t *testing.T) {
	src := makeTestImage(t, 800, 800, imaging.PNG)

	out, err := Resize(src, 300, 300)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	w, h := decodeSize(t, out)
	assert.Equal(t, 300, w)
	assert.Equal(t, 300, h)
}

func TestResize_WebP_Input(t *testing.T) {
	src, err := base64.StdEncoding.DecodeString(webpFixtureB64)
	require.NoError(t, err)

	out, err := Resize(src, 100, 100)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// 200x150 → Fit(100,100): масштаб по ширине 0.5 → 100x75.
	w, h := decodeSize(t, out)
	assert.Equal(t, 100, w)
	assert.Equal(t, 75, h)
}

func TestResize_AspectRatio_500x300_To_100x100(t *testing.T) {
	src := makeTestImage(t, 500, 300, imaging.JPEG)

	out, err := Resize(src, 100, 100)
	require.NoError(t, err)

	// Fit вписывает по большей стороне: 100x60, пропорции сохранены.
	w, h := decodeSize(t, out)
	assert.Equal(t, 100, w)
	assert.Equal(t, 60, h)
}

func TestGetDimensions_JPEG(t *testing.T) {
	src := makeTestImage(t, 800, 600, imaging.JPEG)

	w, h, err := GetDimensions(src)
	require.NoError(t, err)
	assert.Equal(t, 800, w)
	assert.Equal(t, 600, h)
}

func TestGetDimensions_PNG(t *testing.T) {
	src := makeTestImage(t, 123, 456, imaging.PNG)

	w, h, err := GetDimensions(src)
	require.NoError(t, err)
	assert.Equal(t, 123, w)
	assert.Equal(t, 456, h)
}

func TestGetDimensions_WebP(t *testing.T) {
	src, err := base64.StdEncoding.DecodeString(webpFixtureB64)
	require.NoError(t, err)

	// Фикстура — WebP 200x150.
	w, h, err := GetDimensions(src)
	require.NoError(t, err)
	assert.Equal(t, 200, w)
	assert.Equal(t, 150, h)
}

func TestGetDimensions_InvalidData(t *testing.T) {
	_, _, err := GetDimensions([]byte("not an image"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: decode config")
}

func TestGetDimensions_EmptyData(t *testing.T) {
	_, _, err := GetDimensions(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: decode config")
}

func TestResize_InvalidData(t *testing.T) {
	_, err := Resize([]byte("not an image"), 100, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: decode")
}

func TestResize_InvalidSize(t *testing.T) {
	src := makeTestImage(t, 100, 100, imaging.JPEG)

	_, err := Resize(src, 0, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: invalid size")
}

func TestResizeBatch_Success(t *testing.T) {
	src := makeTestImage(t, 1000, 1000, imaging.JPEG)

	out, err := ResizeBatch(src, DefaultSizes)
	require.NoError(t, err)
	require.Len(t, out, 2)

	for name, size := range DefaultSizes {
		data, ok := out[name]
		require.True(t, ok, "нет миниатюры %q", name)
		require.NotEmpty(t, data)

		w, h := decodeSize(t, data)
		assert.LessOrEqual(t, w, size.Width, "миниатюра %q шире рамки", name)
		assert.LessOrEqual(t, h, size.Height, "миниатюра %q выше рамки", name)
	}
}

func TestResizeBatch_EmptySizes(t *testing.T) {
	src := makeTestImage(t, 100, 100, imaging.JPEG)

	_, err := ResizeBatch(src, map[string]Size{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: empty sizes")
}

func TestResizeBatch_InvalidSize(t *testing.T) {
	src := makeTestImage(t, 100, 100, imaging.JPEG)

	_, err := ResizeBatch(src, map[string]Size{"bad": {Width: 0, Height: 100}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resizer: invalid size")
}
