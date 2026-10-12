package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/b602op/avatars-service/internal/handlers"
)

// requestTimeout — верхняя граница времени обработки одного запроса.
const requestTimeout = 30 * time.Second

// staticDir — директория со статикой веб-интерфейса (относительно рабочей).
const staticDir = "web/static"

// RouterDeps — зависимости роутера.
//
// На 6.2 содержит только логгер; сервисы (аватарки) добавятся на 6.3.
type RouterDeps struct {
	Log *slog.Logger
}

// NewRouter собирает Chi-роутер с middleware и маршрутами.
func NewRouter(deps RouterDeps) *chi.Mux {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}

	r := chi.NewRouter()

	// Порядок middleware: снаружи внутрь. RequestID — первым, чтобы request_id
	// был доступен логгеру; Recoverer — внутри логгера, чтобы паника попала в лог
	// как 500.
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(RequestLogger(log))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestTimeout))

	// Health-check — публичный маршрут (без X-User-ID).
	r.Get("/health", handlers.Health())

	// Раздача статики веб-интерфейса.
	r.Handle("/web/static/*", http.StripPrefix("/web/static/", http.FileServer(http.Dir(staticDir))))

	return r
}
