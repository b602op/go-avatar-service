// Package api настраивает HTTP-роутер (Chi), middleware и маршруты.
//
// Хелперы контекста (userID) вынесены в internal/httpx, чтобы избежать
// циклического импорта между api и handlers.
package api
