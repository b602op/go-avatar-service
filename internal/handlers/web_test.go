package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServeUploadForm_GET(t *testing.T) {
	// Страница отдаётся относительно корня проекта; тесты запускаются из
	// internal/handlers/, поэтому поднимаемся на два уровня.
	t.Chdir("../../")

	rec := httptest.NewRecorder()
	ServeUploadForm(rec, httptest.NewRequest(http.MethodGet, "/web/upload", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Avatar Upload Service")
}

func TestServeUploadForm_POST(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeUploadForm(rec, httptest.NewRequest(http.MethodPost, "/web/upload", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestRedirectToGallery(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/web/gallery/{user_id}", RedirectToGallery)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/web/gallery/user-123", nil))

	require.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, "/api/v1/users/user-123/avatars", rec.Header().Get("Location"))
}
