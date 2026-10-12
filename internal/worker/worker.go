package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/services/publisher"
)

// Worker оркестрирует consumer'ов: запускает их и корректно останавливает.
type Worker struct {
	conn      *amqp.Connection
	handler   *Handler
	cleanup   *Cleanup
	log       *slog.Logger
	consumers []*Consumer
}

// NewWorker подключается к RabbitMQ и подписывается на очереди загрузки и удаления.
//
// cleanup может быть nil — тогда фоновая перепубликация зависших pending-аватарок
// не запускается.
func NewWorker(cfg config.RabbitMQConfig, handler *Handler, cleanup *Cleanup, log *slog.Logger) (*Worker, error) {
	if log == nil {
		log = slog.Default()
	}

	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("worker: dial: %w", err)
	}

	// Воркер объявляет топологию сам, чтобы стартовать независимо от сервера:
	// иначе consume падает с 404 NOT_FOUND на «свежем» брокере. Используется та же
	// функция, что и у паблишера, — аргументы очередей обязаны совпадать.
	topoCh, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("worker: open topology channel: %w", err)
	}
	if err := publisher.DeclareTopology(topoCh, cfg.Exchange, cfg.RetryTTL); err != nil {
		_ = topoCh.Close()
		_ = conn.Close()
		return nil, err
	}
	_ = topoCh.Close()

	consumers := make([]*Consumer, 0, 2)
	upload, err := NewConsumer(conn, publisher.QueueAvatarUploaded, publisher.RetryExchange,
		publisher.RoutingKeyRetryUploaded, uploadHandlerFunc(handler), log)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	consumers = append(consumers, upload)

	del, err := NewConsumer(conn, publisher.QueueAvatarDeleted, publisher.RetryExchange,
		publisher.RoutingKeyRetryDeleted, deleteHandlerFunc(handler), log)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	consumers = append(consumers, del)

	return &Worker{conn: conn, handler: handler, cleanup: cleanup, log: log, consumers: consumers}, nil
}

// uploadHandlerFunc разбирает AvatarUploadEvent и передаёт его в Handler.
//
// Невалидный JSON — PermanentError: повтор разбора бессмыслен, сообщение в DLQ.
func uploadHandlerFunc(h *Handler) MessageHandler {
	return func(ctx context.Context, body []byte) error {
		var event publisher.AvatarUploadEvent
		if err := json.Unmarshal(body, &event); err != nil {
			return &publisher.PermanentError{Err: fmt.Errorf("worker: unmarshal upload event: %w", err)}
		}
		return h.HandleUpload(ctx, event)
	}
}

// deleteHandlerFunc разбирает AvatarDeleteEvent и передаёт его в Handler.
func deleteHandlerFunc(h *Handler) MessageHandler {
	return func(ctx context.Context, body []byte) error {
		var event publisher.AvatarDeleteEvent
		if err := json.Unmarshal(body, &event); err != nil {
			return &publisher.PermanentError{Err: fmt.Errorf("worker: unmarshal delete event: %w", err)}
		}
		return h.HandleDelete(ctx, event)
	}
}

// Run запускает cleanup и всех consumer'ов, затем ждёт отмены контекста или
// падения одного из consumer'ов.
func (w *Worker) Run(ctx context.Context) error {
	if w.cleanup != nil {
		go w.cleanup.Run(ctx)
	}

	errChan := make(chan error, len(w.consumers))
	for _, c := range w.consumers {
		go runConsumer(ctx, c, errChan)
	}

	select {
	case <-ctx.Done():
		w.log.Info("shutting down worker...")
		w.shutdownConsumers()
		return nil
	case err := <-errChan:
		w.log.Error("consumer failed, shutting down", "error", err)
		w.shutdownConsumers()
		return err
	}
}

// runConsumer оборачивает Consumer.Run: ловит панику и сообщает о падении.
func runConsumer(ctx context.Context, c *Consumer, errChan chan<- error) {
	defer func() {
		if r := recover(); r != nil {
			errChan <- fmt.Errorf("worker: panic in consumer %q: %v", c.queue, r)
		}
	}()

	if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		errChan <- err
	}
}

// Close останавливает consumer'ов и закрывает соединение (для graceful shutdown).
func (w *Worker) Close() error {
	w.shutdownConsumers()
	return nil
}

// shutdownConsumers идемпотентно закрывает каналы consumer'ов и соединение.
func (w *Worker) shutdownConsumers() {
	for _, c := range w.consumers {
		if err := c.Close(); err != nil {
			w.log.Error("failed to close consumer", "queue", c.queue, "error", err)
		}
	}
	if w.conn != nil && !w.conn.IsClosed() {
		if err := w.conn.Close(); err != nil {
			w.log.Error("failed to close rabbitmq connection", "error", err)
		}
	}
}
