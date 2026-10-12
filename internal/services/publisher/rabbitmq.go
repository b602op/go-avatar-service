package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
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

// RabbitMQPublisher публикует события аватарок в RabbitMQ.
type RabbitMQPublisher struct {
	conn     *amqp.Connection
	channel  *amqp.Channel
	exchange string
	log      *slog.Logger
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
			"x-message-ttl":             int32(retryTTL / time.Millisecond),
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
func (p *RabbitMQPublisher) publish(ctx context.Context, routingKey, messageID string, event any) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("publisher: marshal %s event: %w", routingKey, err)
	}

	msg := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    messageID, // для идемпотентности воркера
		Timestamp:    time.Now(),
		Body:         body,
	}

	if err := p.channel.PublishWithContext(ctx, p.exchange, routingKey, false, false, msg); err != nil {
		return fmt.Errorf("publisher: publish %s: %w", routingKey, err)
	}

	p.log.Debug("published "+routingKey, "message_id", messageID)
	return nil
}
