package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/pkg/resizer"
)

// ─── стабы для быстрых тестов (без RabbitMQ) ────────────────────────────────

// procUpdate — зафиксированный вызов UpdateProcessingStatus.
type procUpdate struct {
	id     uuid.UUID
	status domain.ProcessingStatus
}

// stubRepo — минимальная реализация AvatarRepository; фиксирует вызовы GetByID
// и UpdateProcessingStatus.
type stubRepo struct {
	getByIDFn     func(ctx context.Context, id uuid.UUID) (*domain.Avatar, error)
	listPendingFn func(ctx context.Context, olderThan time.Duration) ([]*domain.Avatar, error)
	updateProcFn  func(ctx context.Context, id uuid.UUID, status domain.ProcessingStatus) error
	calls         []uuid.UUID
	procUpdates   []procUpdate
}

func (s *stubRepo) Create(context.Context, *domain.Avatar) error { return nil }

func (s *stubRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error) {
	s.calls = append(s.calls, id)
	if s.getByIDFn != nil {
		return s.getByIDFn(ctx, id)
	}
	return nil, domain.ErrNotFound
}

func (s *stubRepo) ListByUserID(context.Context, string) ([]*domain.Avatar, error) { return nil, nil }

func (s *stubRepo) UpdateProcessingStatus(ctx context.Context, id uuid.UUID, status domain.ProcessingStatus, _ map[string]string) error {
	s.procUpdates = append(s.procUpdates, procUpdate{id: id, status: status})
	if s.updateProcFn != nil {
		return s.updateProcFn(ctx, id, status)
	}
	return nil
}

// statusesFor возвращает последовательность статусов, записанных для аватара.
func (s *stubRepo) statusesFor(id uuid.UUID) []domain.ProcessingStatus {
	var out []domain.ProcessingStatus
	for _, u := range s.procUpdates {
		if u.id == id {
			out = append(out, u.status)
		}
	}
	return out
}

func (s *stubRepo) UpdateUploadStatus(context.Context, uuid.UUID, domain.UploadStatus) error {
	return nil
}

func (s *stubRepo) SoftDelete(context.Context, uuid.UUID) error { return nil }

func (s *stubRepo) ListPending(ctx context.Context, olderThan time.Duration) ([]*domain.Avatar, error) {
	if s.listPendingFn != nil {
		return s.listPendingFn(ctx, olderThan)
	}
	return nil, nil
}

// stubStorage — заглушка ObjectStorage для быстрых тестов.
type stubStorage struct{}

func (stubStorage) EnsureBucket(context.Context) error { return nil }

func (stubStorage) Upload(context.Context, string, io.Reader, int64, string) error { return nil }

func (stubStorage) Download(context.Context, string) (io.ReadCloser, *domain.ObjectInfo, error) {
	return io.NopCloser(bytes.NewReader(nil)), &domain.ObjectInfo{}, nil
}

func (stubStorage) Delete(context.Context, string) error { return nil }

func (stubStorage) DeleteMany(context.Context, []string) error { return nil }

func (stubStorage) StatObject(context.Context, string) (*domain.ObjectInfo, error) {
	return nil, domain.ErrNotFound
}

func (stubStorage) HealthCheck(context.Context) error { return nil }

// Проверки на этапе компиляции: стабы реализуют доменные интерфейсы.
var (
	_ domain.AvatarRepository = (*stubRepo)(nil)
	_ domain.ObjectStorage    = stubStorage{}
)

// ─── быстрые тесты: разбор событий ──────────────────────────────────────────

func TestUploadHandlerFunc_ValidJSON(t *testing.T) {
	repo := &stubRepo{}
	h := NewHandler(repo, stubStorage{}, &mockResizer{}, testLogger())

	avatarID := uuid.New()
	body, err := json.Marshal(publisher.AvatarUploadEvent{
		AvatarID: avatarID.String(),
		UserID:   "user-1",
		S3Key:    "originals/user-1/x/photo.jpg",
	})
	require.NoError(t, err)

	// stubRepo отдаёт ErrNotFound → HandleUpload завершается мягко (nil).
	require.NoError(t, uploadHandlerFunc(h)(context.Background(), body))
	require.Len(t, repo.calls, 1)
	assert.Equal(t, avatarID, repo.calls[0])
}

func TestUploadHandlerFunc_InvalidJSON(t *testing.T) {
	h := NewHandler(&stubRepo{}, stubStorage{}, &mockResizer{}, testLogger())

	err := uploadHandlerFunc(h)(context.Background(), []byte("{not json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal upload event")
}

func TestDeleteHandlerFunc_ValidJSON(t *testing.T) {
	h := NewHandler(&stubRepo{}, stubStorage{}, &mockResizer{}, testLogger())

	body, err := json.Marshal(publisher.AvatarDeleteEvent{
		AvatarID: uuid.NewString(),
		UserID:   "user-1",
		S3Keys:   []string{"originals/user-1/x/photo.jpg"},
	})
	require.NoError(t, err)

	require.NoError(t, deleteHandlerFunc(h)(context.Background(), body))
}

func TestDeleteHandlerFunc_InvalidJSON(t *testing.T) {
	h := NewHandler(&stubRepo{}, stubStorage{}, &mockResizer{}, testLogger())

	err := deleteHandlerFunc(h)(context.Background(), []byte("}{"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal delete event")
}

// ─── интеграционные тесты: RabbitMQ ─────────────────────────────────────────

// Параметры тестовой топологии.
//
// Все тесты пакета используют ОДИН exchange и ОДИН TTL: retry-очереди имеют
// фиксированные имена, а их аргументы (TTL, dead-letter-exchange) — часть
// определения очереди. Разные значения у разных тестов дали бы PRECONDITION_FAILED.
const (
	testExchange = "avatars.test.exchange"
	testRetryTTL = 1000 * time.Millisecond
)

// testPublisher — публикатор, работающий на тестовом exchange.
type testPublisher struct {
	pub      *publisher.RabbitMQPublisher
	exchange string
}

// newTestPublisher объявляет тестовую топологию и чистит очереди.
func newTestPublisher(t *testing.T) *testPublisher {
	t.Helper()
	requireInfra(t)

	pub, err := publisher.NewRabbitMQ(config.RabbitMQConfig{
		URL:      rabbitURL,
		Exchange: testExchange,
		RetryTTL: testRetryTTL,
	}, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	purgeQueues(t)
	return &testPublisher{pub: pub, exchange: testExchange}
}

// dial открывает отдельное AMQP-соединение для теста.
func dial(t *testing.T) *amqp.Connection {
	t.Helper()

	conn, err := amqp.Dial(rabbitURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// purgeQueues удаляет накопленные сообщения из всех очередей (основных, DLQ и retry).
func purgeQueues(t *testing.T) {
	t.Helper()

	conn := dial(t)
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()

	queues := []string{
		publisher.QueueAvatarUploaded,
		publisher.QueueAvatarDeleted,
		publisher.QueueAvatarUploadedDLQ,
		publisher.QueueAvatarDeletedDLQ,
		publisher.QueueAvatarUploadedRetry,
		publisher.QueueAvatarDeletedRetry,
	}
	for _, q := range queues {
		_, err := ch.QueuePurge(q, false)
		require.NoError(t, err, "не удалось очистить очередь %q", q)
	}
}

// publishRaw публикует произвольные байты (для теста Nack).
func publishRaw(t *testing.T, exchange, routingKey string, body []byte) {
	t.Helper()

	conn := dial(t)
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()

	err = ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
	})
	require.NoError(t, err)
}

// queueCounter возвращает функцию подсчёта сообщений в очереди. Одно соединение
// и один канал переиспользуются: опрос в цикле не должен плодить подключения.
func queueCounter(t *testing.T) func(queue string) int {
	t.Helper()

	conn := dial(t)
	ch, err := conn.Channel()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ch.Close() })

	return func(queue string) int {
		info, err := ch.QueueDeclarePassive(queue, false, false, false, false, nil)
		require.NoError(t, err)
		return info.Messages
	}
}

// startConsumer запускает consumer в горутине и возвращает функцию остановки.
func startConsumer(t *testing.T, ctx context.Context, queue, retryKey string, handler MessageHandler) func() {
	t.Helper()

	conn := dial(t)
	c, err := NewConsumer(conn, queue, publisher.RetryExchange, retryKey, handler, testLogger())
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(runCtx)
	}()

	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("consumer не остановился за 5 секунд")
		}
		_ = c.Close()
	}
}

// waitFor ждёт выполнения условия или падает по таймауту.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("условие не выполнено за %s: %s", timeout, msg)
}

func TestConsumer_ProcessesUploadEvent(t *testing.T) {
	env, ctx := setupTest(t)
	tp := newTestPublisher(t)
	ql := queueCounter(t)

	a := newPendingAvatar(t, env, ctx)
	uploadOriginal(t, env, ctx, a, makeJPEG(t, 400, 400))

	h := NewHandler(env.repo, env.storage, ResizerFunc(resizer.ResizeBatch), testLogger())
	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, uploadHandlerFunc(h))
	defer stop()

	require.NoError(t, tp.pub.PublishAvatarUploaded(ctx, a.ID, a.UserID, a.S3Key))

	// Сообщение обработано: статус completed и ключи миниатюр заполнены.
	waitFor(t, 15*time.Second, func() bool {
		got, err := env.repo.GetByID(ctx, a.ID)
		return err == nil && got.ProcessingStatus == domain.ProcessingStatusCompleted
	}, "аватар не обработан consumer'ом")

	// Ack: сообщение ушло из очереди.
	waitFor(t, 5*time.Second, func() bool {
		return ql(publisher.QueueAvatarUploaded) == 0
	}, "сообщение не подтверждено (очередь не пуста)")
}

func TestConsumer_NacksInvalidMessage(t *testing.T) {
	env, ctx := setupTest(t)
	tp := newTestPublisher(t)
	ql := queueCounter(t)

	h := NewHandler(env.repo, env.storage, &mockResizer{}, testLogger())
	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, uploadHandlerFunc(h))
	defer stop()

	publishRaw(t, tp.exchange, publisher.RoutingKeyAvatarUploaded, []byte("не json"))

	// Невалидный JSON — PermanentError → Nack(requeue=false) → DLQ.
	waitFor(t, 10*time.Second, func() bool {
		return ql(publisher.QueueAvatarUploaded) == 0
	}, "сообщение не отклонено (очередь не пуста)")
}

func TestConsumer_ProcessesDeleteEvent(t *testing.T) {
	env, ctx := setupTest(t)
	tp := newTestPublisher(t)

	keys := []string{
		"originals/user-1/" + uuid.NewString() + "/photo.jpg",
		"thumbnails/user-1/" + uuid.NewString() + "/100x100.jpg",
	}
	for _, k := range keys {
		data := []byte(k)
		require.NoError(t, env.storage.Upload(ctx, k, bytes.NewReader(data), int64(len(data)), "image/jpeg"))
	}

	h := NewHandler(env.repo, env.storage, &mockResizer{}, testLogger())
	stop := startConsumer(t, ctx, publisher.QueueAvatarDeleted, publisher.RoutingKeyRetryDeleted, deleteHandlerFunc(h))
	defer stop()

	require.NoError(t, tp.pub.PublishAvatarDeleted(ctx, uuid.New(), "user-1", keys))

	waitFor(t, 15*time.Second, func() bool {
		_, err := env.storage.StatObject(ctx, keys[0])
		return err != nil
	}, "файлы не удалены consumer'ом")

	for _, k := range keys {
		_, err := env.storage.StatObject(ctx, k)
		assert.ErrorIs(t, err, domain.ErrNotFound, "объект %q не удалён", k)
	}
}

// ─── DLQ / retry ────────────────────────────────────────────────────────────

func TestConsumer_PermanentError_NacksToDLQ(t *testing.T) {
	requireInfra(t)
	tp := newTestPublisher(t)
	ql := queueCounter(t)
	ctx := context.Background()

	// Обработчик всегда отдаёт PermanentError — сообщение обязано уйти в DLQ.
	handler := func(context.Context, []byte) error {
		return &publisher.PermanentError{Err: errors.New("bad message")}
	}
	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, handler)
	defer stop()

	publishRaw(t, tp.exchange, publisher.RoutingKeyAvatarUploaded, []byte("{}"))

	waitFor(t, 10*time.Second, func() bool {
		return ql(publisher.QueueAvatarUploadedDLQ) == 1
	}, "сообщение не попало в DLQ")

	assert.Equal(t, 0, ql(publisher.QueueAvatarUploaded), "основная очередь должна быть пуста")
}

// TestConsumer_TransientError_PublishesToRetryQueue проверяет отложенный повтор:
// transient-ошибка кладёт сообщение в retry-очередь (TTL), откуда оно через
// dead-lettering возвращается в основную очередь.
func TestConsumer_TransientError_PublishesToRetryQueue(t *testing.T) {
	requireInfra(t)
	tp := newTestPublisher(t)
	ql := queueCounter(t)
	ctx := context.Background()

	// Первая попытка падает (transient), вторая — успешна.
	var calls int32
	handler := func(context.Context, []byte) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, handler)
	defer stop()

	publishRaw(t, tp.exchange, publisher.RoutingKeyAvatarUploaded, []byte("{}"))

	// После первой ошибки сообщение должно оказаться в retry-очереди
	// (окно до истечения TTL).
	waitFor(t, 900*time.Millisecond, func() bool {
		return ql(publisher.QueueAvatarUploadedRetry) == 1
	}, "сообщение не попало в retry-очередь")

	// Через TTL оно возвращается в основную очередь и обрабатывается успешно.
	waitFor(t, 10*time.Second, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	}, "сообщение не вернулось из retry-очереди")

	assert.Equal(t, 0, ql(publisher.QueueAvatarUploadedDLQ), "transient-ошибка не должна попадать в DLQ")
}

// TestConsumer_MaxRetries_DLQ проверяет полный цикл: 3 повтора через retry-очередь,
// затем сообщение уходит в DLQ.
func TestConsumer_MaxRetries_DLQ(t *testing.T) {
	requireInfra(t)
	tp := newTestPublisher(t)
	ql := queueCounter(t)
	ctx := context.Background()

	var calls int32
	handler := func(context.Context, []byte) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("always transient")
	}
	stop := startConsumer(t, ctx, publisher.QueueAvatarUploaded, publisher.RoutingKeyRetryUploaded, handler)
	defer stop()

	publishRaw(t, tp.exchange, publisher.RoutingKeyAvatarUploaded, []byte("{}"))

	waitFor(t, 30*time.Second, func() bool {
		return ql(publisher.QueueAvatarUploadedDLQ) == 1
	}, "сообщение не попало в DLQ после исчерпания повторов")

	// Первая попытка + maxRetries повторов.
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(maxRetries+1))
	assert.Equal(t, 0, ql(publisher.QueueAvatarUploaded), "основная очередь должна быть пуста")
}

// TestCountRetries проверяет разбор заголовка x-death (чистая логика).
func TestCountRetries(t *testing.T) {
	tests := []struct {
		name    string
		headers amqp.Table
		want    int
	}{
		{"nil headers", nil, 0},
		{"no x-death", amqp.Table{"other": 1}, 0},
		{"wrong type", amqp.Table{"x-death": "oops"}, 0},
		{"single entry", amqp.Table{"x-death": []interface{}{amqp.Table{"count": int64(1)}}}, 1},
		{"multiple entries", amqp.Table{"x-death": []interface{}{
			amqp.Table{"count": int64(2)},
			amqp.Table{"count": int64(3)},
		}}, 5},
		{"non-table entry ignored", amqp.Table{"x-death": []interface{}{"junk"}}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, countRetries(tt.headers))
		})
	}
}

// requireInfra пропускает тест, если контейнеры недоступны.
func requireInfra(t *testing.T) {
	t.Helper()
	if !infraReady {
		t.Skip("postgres/silo/rabbitmq are not available")
	}
}
