// Package logger предоставляет обёртку над log/slog.
package logger

import (
	"log/slog"
	"os"
)

// New создаёт slog.Logger с JSON-хендлером, пишущим в os.Stdout.
// Уровень задаётся строкой (debug|info|warn|error); при неизвестном
// значении используется info.
func New(level string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	return slog.New(handler)
}

// parseLevel маппит строковый уровень логирования в slog.Level.
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
