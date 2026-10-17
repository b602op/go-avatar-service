// Package amqpx содержит общие адаптеры для AMQP (RabbitMQ).
//
// Пакет не зависит от кода сервиса: им пользуются и publisher (server), и
// consumer (worker), поэтому он вынесен отдельно, а не продублирован в обоих.
package amqpx

import (
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/propagation"
)

// HeaderCarrier адаптирует amqp.Table к propagation.TextMapCarrier.
//
// amqp.Table — это map[string]interface{}, а TextMapCarrier ожидает работу со
// строками. Значения trace context (traceparent, tracestate) — всегда строки,
// поэтому адаптера достаточно; нестроковые значения (например, x-death)
// возвращаются как отсутствующие и никак не портятся: Set только добавляет
// или перезаписывает ключ.
type HeaderCarrier amqp.Table

// Get возвращает значение заголовка. Нестроковые значения игнорируются:
// trace context в них лежать не может, а паниковать из-за чужого заголовка
// пропагатор не должен.
func (c HeaderCarrier) Get(key string) string {
	if v, ok := c[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Set записывает заголовок. Карта должна быть инициализирована: nil-карта
// приведёт к панике, поэтому вызывающая сторона обязана создать amqp.Table.
func (c HeaderCarrier) Set(key, value string) {
	c[key] = value
}

// Keys возвращает список заголовков сообщения.
func (c HeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Проверка на этапе компиляции, что адаптер реализует интерфейс пропагатора.
var _ propagation.TextMapCarrier = HeaderCarrier(nil)
