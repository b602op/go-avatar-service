package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/b602op/avatars-service/internal/handlers"
)

// requestTimeout — верхняя граница времени обработки одного запроса.
const requestTimeout = 30 * time.Second

// otelOperation — имя операции для спанов HTTP-сервера. otelhttp уточняет имя
// спана до "METHOD {маршрут}", как только Chi проставит r.Pattern, поэтому в
// Jaeger виден шаблон маршрута, а не конкретный URL.
const otelOperation = "http.server"

// staticDir — директория со статикой веб-интерфейса (относительно рабочей).
const staticDir = "web/static"

// RouterDeps — зависимости роутера.
//
// Хендлер аватарок передаётся готовым (DI): создание сервиса и хендлера —
// ответственность main.go, где собраны все зависимости.
type RouterDeps struct {
	Log           *slog.Logger
	AvatarHandler *handlers.AvatarHandler
	// Health — готовый хендлер /health. Собирается в main.go, потому что
	// проверки требуют pool, storage и broker, недоступных уровню api.
	Health http.HandlerFunc
	// AllowedOrigins — список разрешённых Origin для CORS ("*" — все).
	AllowedOrigins []string
	// RateLimitRPS и RateLimitBurst — параметры общего лимита частоты запросов
	// (см. RateLimit). Нулевые значения отключают лимит.
	RateLimitRPS   float64
	RateLimitBurst int
}

// NewRouter собирает Chi-роутер с middleware и маршрутами.
func NewRouter(deps RouterDeps) *chi.Mux {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}

	r := chi.NewRouter()

	// Порядок middleware — снаружи внутрь, и он значим:
	//
	//  1. RequestID — request_id попадает в логи и в атрибуты спана;
	//  2. RealIP — подменяет RemoteAddr до логирования;
	//  3. otelhttp — создаёт спан на запрос. Всё, что зарегистрировано ниже,
	//     видит активный span, поэтому логи получают trace_id/span_id.
	//     Служебные /health и /live исключены из трейсов (это не бизнес-трафик:
	//     kubelet опрашивает их каждые 5–15 секунд, иначе Jaeger забивается
	//     мусором). Метрики по ним при этом считаются — фильтр действует
	//     только на спаны;
	//  4. RequestLogger — пишет лог уже с trace_id;
	//  5. RateLimit — отклонённые запросы (429) попадают в лог и метрики, а
	//     лежащие под ним зависимости не получают трафика вообще;
	//  6. Recoverer — паника попадает в лог как 500, а не как разорванный ответ;
	//  7. Timeout — верхняя граница обработки запроса;
	//  8. Metrics — HTTP-метрики (RED). Обязательно после otelhttp: шаблон
	//     маршрута доступен только после матчинга в Chi, то есть на возврате
	//     из next.ServeHTTP;
	//  9. CORS — заголовки ставятся до ухода в хендлер.
	r.Use(middleware.RequestID)
	// RealIP подменяет r.RemoteAddr значением из X-Forwarded-For / X-Real-IP.
	// В chi v5 он помечен deprecated (GHSA-3fxj-6jh8-hvhx, GHSA-rjr7-jggh-pgcp,
	// GHSA-9g5q-2w5x-hmxf): без списка доверенных прокси эти заголовки можно
	// подделать, и в лог попадёт чужой IP.
	//
	// Оставляем осознанно: сервис работает за прокси, а RemoteAddr используется
	// только для логирования (не для авторизации или лимитов). Корректное
	// решение — доверять заголовкам лишь от известных прокси-адресов — требует
	// отдельной настройки и вынесено в техдолг шага 11.1.
	r.Use(middleware.RealIP) //nolint:staticcheck // SA1019: см. комментарий выше
	r.Use(otelhttp.NewMiddleware(otelOperation,
		otelhttp.WithFilter(func(r *http.Request) bool {
			return !isServicePath(r.URL.Path)
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			if p := chi.RouteContext(req.Context()).RoutePattern(); p != "" {
				return req.Method + " " + p
			}
			return req.Method + " " + req.URL.Path
		}),
	))
	r.Use(RequestLogger(log))
	r.Use(RateLimit(deps.RateLimitRPS, deps.RateLimitBurst))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestTimeout))
	r.Use(Metrics())
	r.Use(CORS(deps.AllowedOrigins))

	// Публичные маршруты (без X-User-ID).
	//
	// Если хендлер /health не передан, подставляем настоящий с nil-зависимостями:
	// он честно ответит «down». Иначе роутер упал бы с nil-паникой на запросе.
	health := deps.Health
	if health == nil {
		log.Warn("health handler is not configured, using degraded stub")
		health = handlers.Health(nil, nil, nil, log)
	}
	r.Get("/health", health)

	// /live — liveness-проба: «процесс жив и отвечает на HTTP». Зависимости не
	// проверяются, поэтому эндпоинт не может упасть из-за S3/БД/брокера.
	//
	// Разделение принципиальное: /health используется как readinessProbe
	// (падение зависимости выводит под из балансировки, и это правильно),
	// /live — как livenessProbe. Если liveness повесить на /health, падение S3
	// начнёт перезапускать поды: процесс-то жив, а старт приложения требует
	// доступного S3 (EnsureBucket), поэтому рестарт ведёт в CrashLoopBackOff.
	r.Get("/live", handlers.Live)

	// Корень отдаёт форму загрузки: без этого открытие http://host:8080/
	// чтобы у страницы оставался один канонический адрес /web/upload.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/upload", http.StatusFound)
	})

	// Веб-интерфейс.
	r.Get("/web/upload", handlers.ServeUploadForm)
	r.Get("/web/gallery/{user_id}", handlers.RedirectToGallery)
	r.Handle("/web/static/*", http.StripPrefix("/web/static/", http.FileServer(http.Dir(staticDir))))

	// REST API под /api/v1: все маршруты требуют валидный X-User-ID.
	//
	// Маршруты регистрируются с полным относительным путём (а не вложенными
	// r.Route со "/"), чтобы POST /api/v1/avatars работал без завершающего слэша —
	// именно так его вызывает фронтенд (web/static/index.html).
	r.Route("/api/v1", func(r chi.Router) {
		// ─── GET-эндпоинты (публичные, без X-User-ID) ───
		r.Get("/avatars/{avatar_id}", deps.AvatarHandler.GetByID)
		r.Get("/avatars/{avatar_id}/content", deps.AvatarHandler.GetContent)
		r.Get("/avatars/{avatar_id}/metadata", deps.AvatarHandler.Metadata)
		r.Get("/users/{user_id}/avatar", deps.AvatarHandler.GetUserAvatar)
		r.Get("/users/{user_id}/avatars", deps.AvatarHandler.ListByUser)

		// ─── Мутации (POST, DELETE) — требуют X-User-ID ───
		r.Group(func(r chi.Router) {
			r.Use(UserID())

			r.Post("/avatars", deps.AvatarHandler.Upload)
			r.Delete("/avatars/{avatar_id}", deps.AvatarHandler.Delete)
			r.Delete("/users/{user_id}/avatar", deps.AvatarHandler.DeleteUserAvatar)
		})
	})

	return r
}
