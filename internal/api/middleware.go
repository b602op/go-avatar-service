package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Параметры заголовка X-User-ID.
const (
	userIDHeader = "X-User-ID"
	maxUserIDLen = 255
)

// userIDPattern — допустимые символы идентификатора: латиница, цифры, _ и -.
var userIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Проверки на этапе компиляции: middleware возвращают стандартную сигнатуру.
var (
	_ func(http.Handler) http.Handler = RequestLogger(slog.Default())
	_ func(http.Handler) http.Handler = UserID()
)

// RequestLogger логирует каждый HTTP-запрос.
//
// Успешные запросы (status < 500) пишутся на уровне Info, серверные ошибки —
// на уровне Error. Поля: method, path, status, duration, request_id, remote_addr.
// Требует, чтобы middleware.RequestID был установлен раньше (для request_id).
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			status := ww.Status()
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", time.Since(start),
				"request_id", middleware.GetReqID(r.Context()),
				"remote_addr", r.RemoteAddr,
			}
			if status >= http.StatusInternalServerError {
				log.Error("http request", attrs...)
			} else {
				log.Info("http request", attrs...)
			}
		})
	}
}

// UserID читает и валидирует заголовок X-User-ID, кладя значение в контекст.
//
// Не применяется к публичным маршрутам (health, статика). При невалидном
// заголовке отвечает 400 с JSON {"error": "invalid X-User-ID"} и прерывает цепочку.
func UserID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := r.Header.Get(userIDHeader)
			if !validUserID(userID) {
				writeJSONError(w, http.StatusBadRequest, "invalid X-User-ID")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), userID)))
		})
	}
}

// validUserID проверяет: непустой, не длиннее maxUserIDLen и только допустимые символы.
func validUserID(userID string) bool {
	if userID == "" || len(userID) > maxUserIDLen {
		return false
	}
	return userIDPattern.MatchString(userID)
}

// writeJSONError отправляет JSON-ответ с ошибкой.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
