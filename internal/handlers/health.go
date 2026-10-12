package handlers

import (
	"encoding/json"
	"net/http"
)

// Проверка на этапе компиляции: Health возвращает http.HandlerFunc.
var _ http.HandlerFunc = Health()

// Health — заглушка health-check.
//
// Возвращает JSON {"status":"ok"}. Проверка БД, S3 и RabbitMQ появится на ШАГЕ 8.
func Health() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
