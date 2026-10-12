package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/b602op/avatars-service/internal/services/publisher"
)

// maxRetries — сколько раз transient-ошибка возвращает сообщение в очередь,
// прежде чем оно уйдёт в DLQ.
const maxRetries = 3

// MessageHandler обрабатывает тело одного сообщения из очереди.
//
// nil означает успех (сообщение будет ack'нуто), ошибка — провал (Nack).
type MessageHandler func(ctx context.Context, body []byte) error

// Consumer читает одну очередь и передаёт сообщения обработчику.
type Consumer struct {
	conn            *amqp.Connection
	channel         *amqp.Channel
	deliveries      <-chan amqp.Delivery
	queue           string
	retryExchange   string
	retryRoutingKey string
	handler         MessageHandler
	log             *slog.Logger
}

// NewConsumer открывает канал, ограничивает prefetch и подписывается на очередь.
//
// prefetch = 1: воркер берёт по одному сообщению, чтобы не вычитывать очередь
// целиком в память. retryExchange/retryRoutingKey задают, куда публиковать
// сообщение при transient-ошибке (отложенный повтор через TTL retry-очереди).
func NewConsumer(
	conn *amqp.Connection,
	queue, retryExchange, retryRoutingKey string,
	handler MessageHandler,
	log *slog.Logger,
) (*Consumer, error) {
	if log == nil {
		log = slog.Default()
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("worker: open channel: %w", err)
	}

	if err := ch.Qos(1, 0, false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("worker: set qos: %w", err)
	}

	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("worker: consume queue %q: %w", queue, err)
	}

	return &Consumer{
		conn:            conn,
		channel:         ch,
		deliveries:      deliveries,
		queue:           queue,
		retryExchange:   retryExchange,
		retryRoutingKey: retryRoutingKey,
		handler:         handler,
		log:             log,
	}, nil
}

// Run читает сообщения до отмены контекста или закрытия канала доставки.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-c.deliveries:
			if !ok {
				return errors.New("worker: deliveries channel closed")
			}
			c.handleMessage(ctx, msg)
		}
	}
}

// handleMessage вызывает обработчик и подтверждает, откладывает или отправляет
// сообщение в DLQ.
//
// Логика: успех → Ack; PermanentError → Nack(requeue=false) → DLQ; превышен
// лимит повторов → Nack(requeue=false) → DLQ; transient-ошибка → публикация в
// retry-очередь (TTL) + Ack исходного сообщения.
func (c *Consumer) handleMessage(ctx context.Context, msg amqp.Delivery) {
	err := c.handler(ctx, msg.Body)
	if err == nil {
		if ackErr := msg.Ack(false); ackErr != nil {
			c.log.Error("failed to ack message", "queue", c.queue, "error", ackErr)
		}
		return
	}

	var permErr *publisher.PermanentError
	if errors.As(err, &permErr) {
		c.log.Error("permanent error, sending to DLQ",
			"queue", c.queue, "error", err, "message_id", msg.MessageId)
		c.nackToDLQ(msg)
		return
	}

	retryCount := countRetries(msg.Headers)
	if retryCount >= maxRetries {
		c.log.Error("max retries exceeded, sending to DLQ",
			"queue", c.queue, "error", err, "retry_count", retryCount, "message_id", msg.MessageId)
		c.nackToDLQ(msg)
		return
	}

	// Transient-ошибка: откладываем через retry-очередь с TTL.
	c.log.Warn("transient error, sending to retry queue",
		"queue", c.queue, "error", err, "retry_count", retryCount, "message_id", msg.MessageId)

	if pubErr := c.publishToRetry(ctx, msg); pubErr != nil {
		// Retry-публикация не удалась — оставляем сообщение в очереди (worst case).
		c.log.Error("failed to publish to retry queue, requeueing",
			"queue", c.queue, "error", pubErr, "message_id", msg.MessageId)
		if nackErr := msg.Nack(false, true); nackErr != nil {
			c.log.Error("failed to nack message", "queue", c.queue, "error", nackErr)
		}
		return
	}

	// Исходное сообщение подтверждаем: за отложенный повтор отвечает retry-очередь.
	if ackErr := msg.Ack(false); ackErr != nil {
		c.log.Error("failed to ack message", "queue", c.queue, "error", ackErr)
	}
}

// nackToDLQ отклоняет сообщение без возврата в очередь: сработает dead-lettering.
func (c *Consumer) nackToDLQ(msg amqp.Delivery) {
	if nackErr := msg.Nack(false, false); nackErr != nil {
		c.log.Error("failed to nack message", "queue", c.queue, "error", nackErr)
	}
}

// publishToRetry публикует сообщение в retry exchange для отложенного повтора.
//
// Заголовки (включая x-death) копируются, чтобы счётчик повторов не терялся:
// именно dead-lettering из retry-очереди инкрементит x-death.count.
func (c *Consumer) publishToRetry(ctx context.Context, msg amqp.Delivery) error {
	pub := amqp.Publishing{
		ContentType:  msg.ContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    msg.MessageId,
		Timestamp:    time.Now(),
		Headers:      msg.Headers,
		Body:         msg.Body,
	}

	if err := c.channel.PublishWithContext(ctx, c.retryExchange, c.retryRoutingKey, false, false, pub); err != nil {
		return fmt.Errorf("worker: publish to retry: %w", err)
	}
	return nil
}

// countRetries суммирует счётчики x-death из заголовков сообщения.
//
// x-death проставляет RabbitMQ при dead-lettering: это список записей, у каждой
// есть поле count. Отсутствие заголовка означает 0 повторов.
func countRetries(headers amqp.Table) int {
	if headers == nil {
		return 0
	}

	deathRaw, ok := headers["x-death"]
	if !ok {
		return 0
	}

	deaths, ok := deathRaw.([]interface{})
	if !ok {
		return 0
	}

	total := 0
	for _, d := range deaths {
		entry, ok := d.(amqp.Table)
		if !ok {
			continue
		}
		if count, ok := entry["count"].(int64); ok {
			total += int(count)
		}
	}
	return total
}

// Close закрывает канал consumer'а. Повторный вызов безопасен.
func (c *Consumer) Close() error {
	if c.channel == nil || c.channel.IsClosed() {
		return nil
	}
	if err := c.channel.Close(); err != nil {
		return fmt.Errorf("worker: close channel: %w", err)
	}
	return nil
}
