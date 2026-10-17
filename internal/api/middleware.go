package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/b602op/avatars-service/internal/httpx"
	"github.com/b602op/avatars-service/internal/metrics"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Параметры заголовка X-User-ID.
const (
	userIDHeader = "X-User-ID"
	maxUserIDLen = 255
)

// unknownRoute — метка path для запросов, которым не досталось шаблона
// маршрута: 404/405 и всё, что не попало в дерево Chi. Конкретный URL здесь
// использовать нельзя — кардинальность метки выросла бы вместе с данными.
const unknownRoute = "unknown"

// userIDPattern — допустимые символы идентификатора: латиница, цифры, _ и -.
var userIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Проверки на этапе компиляции: middleware возвращают стандартную сигнатуру.
var (
	_ func(http.Handler) http.Handler = RequestLogger(slog.Default())
	_ func(http.Handler) http.Handler = UserID()
	_ func(http.Handler) http.Handler = CORS([]string{"*"})
	_ func(http.Handler) http.Handler = Metrics()
)

// RequestLogger логирует каждый HTTP-запрос.
//
// Успешные запросы (status < 500) пишутся на уровне Info, серверные ошибки —
// на уровне Error. Поля: method, path, status, duration, request_id, remote_addr.
// Требует, чтобы middleware.RequestID был установлен раньше (для request_id).
//
// Логи пишутся через *Context: к этому моменту (после otelhttp) в контексте
// запроса лежит активный спан, и traceAwareHandler добавляет в запись trace_id
// и span_id. Именно это связывает лог с трейсом в Jaeger.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			status := ww.Status()
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", time.Since(start),
				"request_id", middleware.GetReqID(ctx),
				"remote_addr", r.RemoteAddr,
			}
			if status >= http.StatusInternalServerError {
				log.ErrorContext(ctx, "http request", attrs...)
			} else {
				log.InfoContext(ctx, "http request", attrs...)
			}
		})
	}
}

// Metrics собирает HTTP-метрики (RED) и отдаёт их в Prometheus.
//
// Регистрируется ПОСЛЕ otelhttp и до CORS: Chi выполняет middleware в порядке
// объявления, а шаблон маршрута появляется только после матчинга, то есть
// к моменту возврата из next.ServeHTTP. Поэтому chi.RouteContext(...).
// RoutePattern() внутри Metrics уже заполнен и даёт "/api/v1/avatars/{avatar_id}",
// а не конкретный URL — кардинальность метки path остаётся на уровне числа
// маршрутов.
func Metrics() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Тот же wrapper, что и в RequestLogger: в отличие от «голой»
			// структуры он сохраняет у обёрнутого ResponseWriter интерфейсы
			// Flusher/Hijacker/io.ReaderFrom, нужные net/http.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			// Status() == 0, если хендлер не отправил ни заголовков, ни тела:
			// в этом случае net/http сам отвечает 200, поэтому 0 в метрику
			// не пишем.
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}

			path := chi.RouteContext(r.Context()).RoutePattern()
			if path == "" {
				path = unknownRoute
			}

			statusLabel := strconv.Itoa(status)
			metrics.HTTPRequestsTotal.WithLabelValues(r.Method, path, statusLabel).Inc()
			metrics.HTTPRequestDuration.WithLabelValues(r.Method, path, statusLabel).
				Observe(time.Since(start).Seconds())
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
			next.ServeHTTP(w, r.WithContext(httpx.WithUserID(r.Context(), userID)))
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

// CORS добавляет заголовки Cross-Origin Resource Sharing.
//
// Если в allowedOrigins есть "*", разрешаются все источники; иначе — только
// перечисленные. Preflight-запрос (OPTIONS) завершается 204 без тела.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowAll := false
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowed := allowAll || contains(allowedOrigins, origin)

			if origin != "" && allowed {
				if allowAll {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User-ID")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			// Preflight не идёт дальше по цепочке.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// contains проверяет наличие строки в срезе.
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// writeJSONError отправляет JSON-ответ с ошибкой.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
