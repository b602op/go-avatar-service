// Package httpx содержит общие утилиты HTTP-транспорта.
//
// Хелперы контекста вынесены в отдельный пакет, чтобы их могли использовать и
// middleware (internal/api), и хендлеры (internal/handlers) без импортного цикла.
package httpx

import "context"

// contextKey — приватный тип ключей контекста, чтобы избежать коллизий.
type contextKey string

// userIDKey — ключ для идентификатора пользователя в контексте запроса.
const userIDKey contextKey = "userID"

// WithUserID кладёт идентификатор пользователя в контекст.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserIDFromContext извлекает идентификатор пользователя из контекста.
// Второй результат — false, если значение отсутствует или имеет другой тип.
func UserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDKey).(string)
	return userID, ok
}
