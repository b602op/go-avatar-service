package handlers

import (
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
)

// indexFile — путь к HTML-странице веб-интерфейса (относительно рабочей директории).
var indexFile = filepath.Join("web", "static", "index.html")

// ServeUploadForm отдаёт HTML-форму загрузки.
//
// Только GET: форма шлёт данные напрямую на POST /api/v1/avatars.
func ServeUploadForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, indexFile)
}

// RedirectToGallery редиректит на API-эндпоинт со списком аватарок пользователя.
func RedirectToGallery(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "user_id")
	if userID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/api/v1/users/"+userID+"/avatars", http.StatusMovedPermanently)
}
