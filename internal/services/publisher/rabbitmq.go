package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/b602op/avatars-service/internal/amqpx"
	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services/circuitbreaker"
)

// Топология событий аватарок: routing keys и имена очередей.
//
// Экспортированы, чтобы воркер (ШАГ 7.3) подписывался на те же очереди.
const (
	RoutingKeyAvatarUploaded = "avatar.uploaded"
	RoutingKeyAvatarDeleted  = "avatar.deleted"

	QueueAvatarUploaded = "avatar.uploaded.queue"
	QueueAvatarDeleted  = "avatar.deleted.queue"

	// DLXName — dead letter exchange: принимает сообщения, которые не удалось
	// обработать, и раскладывает их по DLQ.
	DLXName = "avatars.dlx"

	// Routing keys, по которым DLX маршрутизирует сообщения в DLQ.
	RoutingKeyAvatarUploadedDLQ = "avatar.uploaded.dlq"
	RoutingKeyAvatarDeletedDLQ  = "avatar.deleted.dlq"

	// Очереди для «мёртвых» сообщений (по одной на основной поток).
	QueueAvatarUploadedDLQ = "avatar.uploaded.queue.dlq"
	QueueAvatarDeletedDLQ  = "avatar.deleted.queue.dlq"

	// RetryExchange — direct exchange для отложенного повтора обработки.
	RetryExchange = "avatars.retry.exchange"

	// Retry-очереди: держат сообщение retryTTL и затем dead-letter'ят его обратно
	// в основную очередь. Каждый такой цикл инкрементит x-death.count.
	QueueAvatarUploadedRetry = "avatar.uploaded.retry.queue"
	QueueAvatarDeletedRetry  = "avatar.deleted.retry.queue"

	// Routing keys для публикации в RetryExchange.
	RoutingKeyRetryUploaded = "retry.uploaded"
	RoutingKeyRetryDeleted  = "retry.deleted"

	// DefaultRetryTTL — TTL retry-очередей по умолчанию (30 секунд).
	DefaultRetryTTL = 30 * time.Second

	// exchangeKind — тип основного exchange; routing keys с точками совместимы с topic.
	exchangeKind = "topic"
	// dlxKind — тип dead letter exchange: маршрутизация по точному routing key.
	dlxKind = "direct"
	// retryExchangeKind — тип retry exchange: маршрутизация по точному routing key.
	retryExchangeKind = "direct"
)

// Проверка на этапе компиляции, что реализация соответствует доменному интерфейсу.
var _ domain.EventPublisher = (*RabbitMQPublisher)(nil)

// tracer — трейсер для спанов публикации: у amqp091-go нет готовой OTel
// инструментации, поэтому спан создаётся вручную, а trace context передаётся
// получателю через заголовки сообщения.
//
// Переменная инициализируется при загрузке пакета, то есть до telemetry.Init.
// Это безопасно: до установки SDK глобальный провайдер OTel отдаёт no-op
// трейсеры и подменяет их на трейсеры SDK в otel.SetTracerProvider.
var tracer = otel.Tracer("github.com/b602op/avatars-service/publisher")

// RabbitMQPublisher публикует события аватарок в RabbitMQ.
type RabbitMQPublisher struct {
	conn     *amqp.Connection
	channel  *amqp.Channel
	exchange string
	log      *slog.Logger

	// breaker защищает публикацию от вызовов к лежащему брокеру: пока он
	// открыт, publish падает сразу с ErrCircuitOpen, не занимая горутину
	// HTTP-запроса ожиданием недоступного RabbitMQ.
	//
	// Подключение и объявление топологии (NewRabbitMQ, DeclareTopology) вне
	// breaker'а: это разовая операция старта, её отказ должен быть виден
	// вызывающему как есть.
	breaker *circuitbreaker.Breaker
}

// NewRabbitMQ подключается к RabbitMQ и объявляет топологию (exchange + очереди).
//
// При ошибке на любом шаге уже открытые ресурсы закрываются, а ошибка
// возвращается наверх — решение о завершении процесса принимает main.
func NewRabbitMQ(cfg config.RabbitMQConfig, log *slog.Logger) (*RabbitMQPublisher, error) {
	if log == nil {
		log = slog.Default()
	}

	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("publisher: dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("publisher: open channel: %w", err)
	}

	// Для MVP используется одно соединение и один канал на publish.
	// В будущем планируется пул каналов или разделение на publish/consume
	// для изоляции ошибок: при падении канала все операции на нём фейлятся.
	if err := DeclareTopology(ch, cfg.Exchange, cfg.RetryTTL); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, err
	}

	log.Info("rabbitmq publisher ready", "exchange", cfg.Exchange)

	return &RabbitMQPublisher{
		conn:     conn,
		channel:  ch,
		exchange: cfg.Exchange,
		log:      log,
		breaker:  circuitbreaker.New("rabbitmq", circuitbreaker.LogStateChanges(log)),
	}, nil
}

// DeclareTopology идемпотентно объявляет топологию событий аватарок:
// основной exchange, dead letter exchange, DLQ, retry exchange, retry-очереди,
// основные очереди и привязки.
//
// Экспортирована намеренно: и сервер (publisher), и воркер (consumer) обязаны
// объявлять ОДИНАКОВЫЕ аргументы очередей. RabbitMQ запрещает менять аргументы
// уже существующей очереди, поэтому расхождение (например, разный retryTTL)
// приводит к PRECONDITION_FAILED при старте второго сервиса.
//
// retryTTL <= 0 подменяется на DefaultRetryTTL.
func DeclareTopology(ch *amqp.Channel, exchange string, retryTTL time.Duration) error {
	if retryTTL <= 0 {
		retryTTL = DefaultRetryTTL
	}

	if err := ch.ExchangeDeclare(exchange, exchangeKind, true, false, false, false, nil); err != nil {
		return fmt.Errorf("publisher: declare exchange %q: %w", exchange, err)
	}

	// Dead letter exchange и очереди для необработанных сообщений.
	if err := ch.ExchangeDeclare(DLXName, dlxKind, true, false, false, false, nil); err != nil {
		return fmt.Errorf("publisher: declare dlx %q: %w", DLXName, err)
	}

	dlqs := []struct {
		queue      string
		routingKey string
	}{
		{QueueAvatarUploadedDLQ, RoutingKeyAvatarUploadedDLQ},
		{QueueAvatarDeletedDLQ, RoutingKeyAvatarDeletedDLQ},
	}
	for _, d := range dlqs {
		if _, err := ch.QueueDeclare(d.queue, true, false, false, false, nil); err != nil {
			return fmt.Errorf("publisher: declare dlq %q: %w", d.queue, err)
		}
		if err := ch.QueueBind(d.queue, d.routingKey, DLXName, false, nil); err != nil {
			return fmt.Errorf("publisher: bind dlq %q: %w", d.queue, err)
		}
	}

	// Retry exchange и retry-очереди: сообщение лежит retryTTL, затем через
	// dead-lettering возвращается в основную очередь (routing key = основной).
	// Dead letter exchange — тот же exchange, что маршрутизирует в main-очереди,
	// иначе возврат не найдёт привязку (в проде это avatars.exchange).
	if err := ch.ExchangeDeclare(RetryExchange, retryExchangeKind, true, false, false, false, nil); err != nil {
		return fmt.Errorf("publisher: declare retry exchange %q: %w", RetryExchange, err)
	}

	// x-message-ttl в AMQP — int32 (миллисекунды), а time.Duration — int64.
	// TTL приходит из конфига; ограничиваем сверху, чтобы конвертация не
	// переполнилась на заведомо ошибочном значении (больше ~24 суток).
	ttlMillis := retryTTL.Milliseconds()
	if ttlMillis > math.MaxInt32 {
		ttlMillis = math.MaxInt32
	}
	ttlArg := int32(ttlMillis) //nolint:gosec // G115: значение ограничено MaxInt32 строкой выше

	retryQueues := []struct {
		queue    string
		retryKey string
		mainKey  string
	}{
		{QueueAvatarUploadedRetry, RoutingKeyRetryUploaded, RoutingKeyAvatarUploaded},
		{QueueAvatarDeletedRetry, RoutingKeyRetryDeleted, RoutingKeyAvatarDeleted},
	}
	for _, r := range retryQueues {
		args := amqp.Table{
			"x-message-ttl":             ttlArg,
			"x-dead-letter-exchange":    exchange,
			"x-dead-letter-routing-key": r.mainKey,
		}
		if _, err := ch.QueueDeclare(r.queue, true, false, false, false, args); err != nil {
			return fmt.Errorf("publisher: declare retry queue %q: %w", r.queue, err)
		}
		if err := ch.QueueBind(r.queue, r.retryKey, RetryExchange, false, nil); err != nil {
			return fmt.Errorf("publisher: bind retry queue %q: %w", r.queue, err)
		}
	}

	// Основные очереди: при Nack(requeue=false) сообщение уходит в свою DLQ.
	mainQueues := []struct {
		queue      string
		routingKey string
		dlqKey     string
	}{
		{QueueAvatarUploaded, RoutingKeyAvatarUploaded, RoutingKeyAvatarUploadedDLQ},
		{QueueAvatarDeleted, RoutingKeyAvatarDeleted, RoutingKeyAvatarDeletedDLQ},
	}
	for _, m := range mainQueues {
		args := amqp.Table{
			"x-dead-letter-exchange":    DLXName,
			"x-dead-letter-routing-key": m.dlqKey,
		}
		if _, err := ch.QueueDeclare(m.queue, true, false, false, false, args); err != nil {
			return fmt.Errorf("publisher: declare queue %q: %w", m.queue, err)
		}
		if err := ch.QueueBind(m.queue, m.routingKey, exchange, false, nil); err != nil {
			return fmt.Errorf("publisher: bind queue %q: %w", m.queue, err)
		}
	}
	return nil
}

// Conn возвращает AMQP-соединение паблишера.
//
// Нужен для health-check: /health проверяет, что соединение с брокером не
// закрыто (amqp.Connection.IsClosed). Владение соединением остаётся за
// паблишером — закрывать его должен только Close.
func (p *RabbitMQPublisher) Conn() *amqp.Connection {
	return p.conn
}

// Close закрывает канал и соединение. Повторный вызов безопасен.
func (p *RabbitMQPublisher) Close() error {
	var errs []error

	if p.channel != nil && !p.channel.IsClosed() {
		if err := p.channel.Close(); err != nil {
			errs = append(errs, fmt.Errorf("publisher: close channel: %w", err))
		}
	}
	if p.conn != nil && !p.conn.IsClosed() {
		if err := p.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("publisher: close connection: %w", err))
		}
	}

	return errors.Join(errs...)
}

// PublishAvatarUploaded публикует событие о загрузке оригинала.
func (p *RabbitMQPublisher) PublishAvatarUploaded(ctx context.Context, avatarID uuid.UUID, userID, s3Key string) error {
	event := AvatarUploadEvent{
		AvatarID:  avatarID.String(),
		UserID:    userID,
		S3Key:     s3Key,
		Timestamp: time.Now().Unix(),
	}
	return p.publish(ctx, RoutingKeyAvatarUploaded, avatarID.String(), event)
}

// PublishAvatarDeleted публикует событие об удалении аватарки.
func (p *RabbitMQPublisher) PublishAvatarDeleted(ctx context.Context, avatarID uuid.UUID, userID string, s3Keys []string) error {
	event := AvatarDeleteEvent{
		AvatarID: avatarID.String(),
		UserID:   userID,
		// Копия среза: защита от гонки с вызывающей стороной, которая может
		// переиспользовать исходный срез после возврата из метода.
		S3Keys:    append([]string(nil), s3Keys...),
		Timestamp: time.Now().Unix(),
	}
	return p.publish(ctx, RoutingKeyAvatarDeleted, avatarID.String(), event)
}

// publish сериализует событие и отправляет его в exchange с заданным routing key.
//
// Помимо публикации, метод кладёт trace context в заголовки сообщения: воркер
// извлечёт его и продолжит тот же трейс (спаны обработки станут дочерними к
// HTTP-запросу на сервере, а не «сиротами»).
func (p *RabbitMQPublisher) publish(ctx context.Context, routingKey, messageID string, event any) (err error) {
	ctx, span := tracer.Start(ctx, "RabbitMQ.Publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemRabbitMQ,
			semconv.MessagingDestinationName(p.exchange),
			semconv.MessagingRabbitMQDestinationRoutingKey(routingKey),
			semconv.MessagingMessageID(messageID),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("publisher: marshal %s event: %w", routingKey, err)
	}

	// Headers создаётся заранее: Inject пишет в карту и на nil-карте паникует.
	// Если у события появятся собственные заголовки, дописывать их нужно сюда же,
	// ДО инжекта, чтобы trace context не затирал чужие ключи (он их и не трогает,
	// но порядок делает это очевидным).
	msg := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    messageID, // для идемпотентности воркера
		Timestamp:    time.Now(),
		Body:         body,
		Headers:      amqp.Table{},
	}

	// Trace context (traceparent/tracestate) — в заголовки: это и есть
	// context propagation между server и worker.
	otel.GetTextMapPropagator().Inject(ctx, amqpx.HeaderCarrier(msg.Headers))

	// В breaker оборачивается только сетевой вызов публикации. Ошибки
	// маршалинга — это ошибка данных, а не деградация брокера, и открывать
	// breaker на них нельзя: иначе битое событие одного вида «сломает»
	// публикацию вообще всех событий.
	err = circuitbreaker.ExecuteNoResult(p.breaker, func() error {
		return p.channel.PublishWithContext(ctx, p.exchange, routingKey, false, false, msg)
	})
	if err != nil {
		return fmt.Errorf("publisher: publish %s: %w", routingKey, err)
	}

	p.log.DebugContext(ctx, "published "+routingKey, "message_id", messageID)
	return nil
}
