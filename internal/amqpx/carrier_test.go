package amqpx

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestProvider ставит глобальные провайдер и пропагатор так же, как это
// делает telemetry.Init в проде: W3C Trace Context + Baggage.
func newTestProvider(t *testing.T) *sdktrace.TracerProvider {
	t.Helper()

	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()

	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})

	return tp
}

func TestHeaderCarrier_GetSetKeys(t *testing.T) {
	headers := amqp.Table{"traceparent": "00-abc-def-01"}
	carrier := HeaderCarrier(headers)

	assert.Equal(t, "00-abc-def-01", carrier.Get("traceparent"))
	assert.Equal(t, "", carrier.Get("missing"))
	assert.Equal(t, []string{"traceparent"}, carrier.Keys())

	carrier.Set("tracestate", "vendor=1")
	assert.Equal(t, "vendor=1", headers["tracestate"])
	assert.ElementsMatch(t, []string{"traceparent", "tracestate"}, carrier.Keys())
}

// TestHeaderCarrier_NonStringIgnored проверяет, что служебные заголовки брокера
// (x-death — вложенная таблица, а не строка) не ломают адаптер.
func TestHeaderCarrier_NonStringIgnored(t *testing.T) {
	carrier := HeaderCarrier(amqp.Table{
		"x-death": []interface{}{amqp.Table{"count": int64(2)}},
		"count":   42,
	})

	assert.Equal(t, "", carrier.Get("x-death"))
	assert.Equal(t, "", carrier.Get("count"))
	assert.ElementsMatch(t, []string{"x-death", "count"}, carrier.Keys())
}

// TestHeaderCarrier_NilGetKeys проверяет, что nil-карта безопасна на чтение:
// сообщение без заголовков — штатный случай (Extract вызывается всегда).
func TestHeaderCarrier_NilGetKeys(t *testing.T) {
	carrier := HeaderCarrier(nil)

	assert.Equal(t, "", carrier.Get("traceparent"))
	assert.Empty(t, carrier.Keys())
}

// TestHeaderCarrier_InjectExtractRoundTrip — главная проверка шага: trace context
// переживает сериализацию в заголовки AMQP, то есть спан получателя становится
// дочерним к спану отправителя.
func TestHeaderCarrier_InjectExtractRoundTrip(t *testing.T) {
	tp := newTestProvider(t)
	tracer := tp.Tracer("carrier-test")

	ctx, parent := tracer.Start(context.Background(), "publish")
	require.True(t, parent.SpanContext().IsValid())

	// Producer: inject в заголовки сообщения.
	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(headers))
	assert.Contains(t, headers, "traceparent", "traceparent должен попасть в заголовки")

	// Consumer: extract из заголовков в новый (базовый) контекст.
	extracted := otel.GetTextMapPropagator().Extract(context.Background(), HeaderCarrier(headers))
	remote := trace.SpanContextFromContext(extracted)

	require.True(t, remote.IsValid(), "trace context не извлёкся из заголовков")
	assert.Equal(t, parent.SpanContext().TraceID(), remote.TraceID())
	assert.Equal(t, parent.SpanContext().SpanID(), remote.SpanID(), "родителем должен быть спан публикации")

	// Спан получателя наследует trace_id публикации.
	_, consumer := tracer.Start(extracted, "consume")
	assert.Equal(t, parent.SpanContext().TraceID(), consumer.SpanContext().TraceID())
}

// TestHeaderCarrier_InjectPreservesExistingHeaders проверяет, что инжект не
// затирает служебные заголовки: x-death нужен для подсчёта повторов.
func TestHeaderCarrier_InjectPreservesExistingHeaders(t *testing.T) {
	newTestProvider(t)

	deaths := []interface{}{amqp.Table{"count": int64(3)}}
	headers := amqp.Table{"x-death": deaths}

	ctx, span := otel.Tracer("carrier-test").Start(context.Background(), "publish")
	defer span.End()

	otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(headers))

	assert.Equal(t, deaths, headers["x-death"], "x-death не должен меняться")
	assert.Contains(t, headers, "traceparent")
}

// TestHeaderCarrier_ExtractWithoutTraceparent — сообщение без trace context
// (например, опубликованное до включения трейсинга): ctx остаётся без trace.
func TestHeaderCarrier_ExtractWithoutTraceparent(t *testing.T) {
	newTestProvider(t)

	ctx := otel.GetTextMapPropagator().Extract(context.Background(), HeaderCarrier(amqp.Table{"x-death": "junk"}))
	assert.False(t, trace.SpanContextFromContext(ctx).IsValid())
}

// TestHeaderCarrier_SpansRecorded — защита от случайного no-op трейсера:
// спаны, записанные через глобальный провайдер, должны доходить до процессора.
func TestHeaderCarrier_SpansRecorded(t *testing.T) {
	prevProvider := otel.GetTracerProvider()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevProvider)
	})

	_, span := otel.Tracer("carrier-test").Start(context.Background(), "op")
	span.End()

	require.Len(t, sr.Ended(), 1)
	assert.Equal(t, "op", sr.Ended()[0].Name())
}
