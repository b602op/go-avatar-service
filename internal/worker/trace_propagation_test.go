package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/b602op/avatars-service/internal/services/publisher"
)

// installRecorder ставит глобальные провайдер и пропагатор так же, как это
// делает telemetry.Init в проде (W3C Trace Context + Baggage), и возвращает
// рекордер завершённых спанов.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
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

	return sr
}

// TestConsumer_PropagatesTraceContext проверяет главное требование шага: трейс
// не рвётся между server и worker. Trace context уезжает в заголовках AMQP,
// consumer извлекает его, и спан обработки становится дочерним к спану
// публикации (а спаны БД/S3 внутри обработчика — потомками этого спана).
func TestConsumer_PropagatesTraceContext(t *testing.T) {
	requireInfra(t)

	tp := newTestPublisher(t)
	sr := installRecorder(t)
	ctx := context.Background()

	// Спан публикации играет роль HTTP-спана сервера.
	pubCtx, pubSpan := otel.Tracer("test").Start(ctx, "POST /api/v1/avatars")

	handlerCtx := make(chan context.Context, 1)
	handler := func(hctx context.Context, _ []byte) error {
		handlerCtx <- hctx
		return nil
	}

	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, handler)
	defer stop()

	require.NoError(t, tp.pub.PublishAvatarUploaded(pubCtx, uuid.New(), "user-1", "originals/user-1/x/photo.jpg"))
	pubSpan.End()

	// Обработчик получил контекст, а не базовый ctx из Run.
	var gotCtx context.Context
	select {
	case gotCtx = <-handlerCtx:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer не обработал сообщение")
	}

	consumeSpanCtx := trace.SpanContextFromContext(gotCtx)
	require.True(t, consumeSpanCtx.IsValid(), "в контексте обработчика нет активного спана")
	assert.Equal(t, pubSpan.SpanContext().TraceID(), consumeSpanCtx.TraceID(),
		"trace_id обработки должен совпадать с trace_id публикации")

	// Спан RabbitMQ.Consume — дочерний к спану публикации RabbitMQ.Publish,
	// который, в свою очередь, вложен в «HTTP-спан» сервера.
	var consumeSpan, publishSpan sdktrace.ReadOnlySpan
	waitFor(t, 10*time.Second, func() bool {
		for _, s := range sr.Ended() {
			switch s.Name() {
			case "RabbitMQ.Consume":
				consumeSpan = s
			case "RabbitMQ.Publish":
				publishSpan = s
			}
		}
		return consumeSpan != nil && publishSpan != nil
	}, "спаны RabbitMQ.Publish/RabbitMQ.Consume не записаны")

	assert.Equal(t, trace.SpanKindConsumer, consumeSpan.SpanKind())
	assert.Equal(t, pubSpan.SpanContext().SpanID(), publishSpan.Parent().SpanID(),
		"RabbitMQ.Publish должен быть дочерним к HTTP-спану")
	assert.Equal(t, publishSpan.SpanContext().SpanID(), consumeSpan.Parent().SpanID(),
		"родителем спана обработки должен быть спан публикации")
	assert.Equal(t, pubSpan.SpanContext().TraceID(), consumeSpan.Parent().TraceID(),
		"обработка в воркере должна остаться в трейсе запроса")
}

// TestConsumer_PropagatesTraceContext_OnRetry проверяет, что трейс переживает
// повторную публикацию: при transient-ошибке сообщение уходит в retry-очередь
// с тем же trace context, и после возврата в основную очередь спан обработки
// остаётся в исходном трейсе.
func TestConsumer_PropagatesTraceContext_OnRetry(t *testing.T) {
	requireInfra(t)

	tp := newTestPublisher(t)
	installRecorder(t)
	ctx := context.Background()

	pubCtx, pubSpan := otel.Tracer("test").Start(ctx, "POST /api/v1/avatars")
	traceID := pubSpan.SpanContext().TraceID()

	handlerCtx := make(chan context.Context, 4)
	attempt := 0
	handler := func(hctx context.Context, _ []byte) error {
		handlerCtx <- hctx
		attempt++
		if attempt == 1 {
			// Первая попытка — transient: сообщение уйдёт в retry-очередь.
			return assert.AnError
		}
		return nil
	}

	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, handler)
	defer stop()

	require.NoError(t, tp.pub.PublishAvatarUploaded(pubCtx, uuid.New(), "user-1", "originals/user-1/x/photo.jpg"))
	pubSpan.End()

	// Обе попытки должны прийти в одном трейсе: второй контекст — результат
	// Extract из заголовков, которые consumer дописал при публикации в retry.
	var second context.Context
	select {
	case <-handlerCtx:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer не обработал сообщение")
	}
	select {
	case second = <-handlerCtx:
	case <-time.After(15 * time.Second):
		t.Fatal("сообщение не вернулось из retry-очереди")
	}

	assert.Equal(t, traceID, trace.SpanContextFromContext(second).TraceID(),
		"после retry трейс должен сохраниться")
}
