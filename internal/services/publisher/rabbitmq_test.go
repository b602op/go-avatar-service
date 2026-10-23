package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/services/circuitbreaker"
)

// rabbitURL — адрес RabbitMQ, поднятого один раз на весь пакет (TestMain).
// Пустая строка означает, что контейнер недоступен: тесты скипаются.
var rabbitURL string

// breakerFailuresToTrip — сколько неудач подряд открывают breaker публикации.
// Держим синхронно с circuitbreaker.failuresToTrip (константа не экспортируется).
const breakerFailuresToTrip = 5

func TestMain(m *testing.M) {
	ctx := context.Background()

	url, cleanup, err := startRabbitMQ(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rabbitmq container unavailable, integration tests will be skipped: %v\n", err)
		os.Exit(m.Run())
	}

	rabbitURL = url
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// startRabbitMQ поднимает контейнер RabbitMQ и возвращает AMQP-адрес и функцию очистки.
func startRabbitMQ(ctx context.Context) (string, func(), error) {
	req := testcontainers.ContainerRequest{
		Image:        "rabbitmq:3-management-alpine",
		ExposedPorts: []string{"5672/tcp"},
		WaitingFor: wait.ForLog("Server startup complete").
			WithStartupTimeout(90 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", nil, err
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, err
	}
	mapped, err := container.MappedPort(ctx, "5672")
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, err
	}

	url := fmt.Sprintf("amqp://guest:guest@%s:%s/", host, mapped.Port())
	return url, func() { _ = container.Terminate(ctx) }, nil
}

// testExchange — фиксированный exchange для тестов пакета.
//
// Уникальный exchange на тест здесь не подходит: retry-очереди имеют
// фиксированные имена, а их dead-letter-exchange — часть определения очереди.
// Разные значения у разных тестов дали бы PRECONDITION_FAILED.
const testExchange = "test.exchange.publisher"

// testEnv — окружение одного теста: publisher, отдельное соединение для чтения
// и тестовый exchange.
type testEnv struct {
	pub      *RabbitMQPublisher
	reader   *amqp.Channel
	exchange string
}

// setupTest создаёт publisher с тестовым exchange и чистые очереди.
//
// Имена очередей фиксированы (продовая топология), поэтому между тестами они
// очищаются через purge.
func setupTest(t *testing.T) *testEnv {
	t.Helper()

	if rabbitURL == "" {
		t.Skip("rabbitmq is not available")
	}

	env := newEnv(t, testExchange)

	queues := []string{
		QueueAvatarUploaded, QueueAvatarDeleted,
		QueueAvatarUploadedDLQ, QueueAvatarDeletedDLQ,
		QueueAvatarUploadedRetry, QueueAvatarDeletedRetry,
	}
	for _, q := range queues {
		_, err := env.reader.QueuePurge(q, false)
		require.NoError(t, err)
	}

	return env
}

// newEnv создаёт publisher и читающий канал для указанного exchange.
func newEnv(t *testing.T, exchange string) *testEnv {
	t.Helper()

	pub, err := NewRabbitMQ(config.RabbitMQConfig{URL: rabbitURL, Exchange: exchange}, testLogger())
	require.NoError(t, err)

	conn, err := amqp.Dial(rabbitURL)
	require.NoError(t, err)

	reader, err := conn.Channel()
	require.NoError(t, err)

	t.Cleanup(func() {
		// Удаляем exchange вместе с его биндингами, чтобы не копить мусор.
		_ = reader.ExchangeDelete(exchange, false, false)
		_ = pub.Close()
		_ = reader.Close()
		_ = conn.Close()
	})

	return &testEnv{pub: pub, reader: reader, exchange: exchange}
}

// testLogger возвращает логгер, пишущий в никуда.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// getMessage читает одно сообщение из очереди, ожидая его появления.
func getMessage(t *testing.T, ch *amqp.Channel, queue string) amqp.Delivery {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if ok {
			return msg
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no message in queue %q within timeout", queue)
	return amqp.Delivery{}
}

func TestRabbitMQPublisher_PublishUploaded(t *testing.T) {
	env := setupTest(t)

	avatarID := uuid.New()
	require.NoError(t, env.pub.PublishAvatarUploaded(context.Background(), avatarID, "user-1", "originals/user-1/a/photo.jpg"))

	msg := getMessage(t, env.reader, QueueAvatarUploaded)

	assert.Equal(t, "application/json", msg.ContentType)
	assert.Equal(t, amqp.Persistent, msg.DeliveryMode)
	assert.Equal(t, avatarID.String(), msg.MessageId)

	var event AvatarUploadEvent
	require.NoError(t, json.Unmarshal(msg.Body, &event))
	assert.Equal(t, avatarID.String(), event.AvatarID)
	assert.Equal(t, "user-1", event.UserID)
	assert.Equal(t, "originals/user-1/a/photo.jpg", event.S3Key)
	assert.Greater(t, event.Timestamp, int64(0))
}

func TestRabbitMQPublisher_PublishDeleted(t *testing.T) {
	env := setupTest(t)

	avatarID := uuid.New()
	keys := []string{"originals/u/a/photo.jpg", "thumbnails/u/a/100x100.jpg"}
	require.NoError(t, env.pub.PublishAvatarDeleted(context.Background(), avatarID, "user-1", keys))

	msg := getMessage(t, env.reader, QueueAvatarDeleted)

	assert.Equal(t, avatarID.String(), msg.MessageId)

	var event AvatarDeleteEvent
	require.NoError(t, json.Unmarshal(msg.Body, &event))
	assert.Equal(t, avatarID.String(), event.AvatarID)
	assert.Equal(t, "user-1", event.UserID)
	assert.Equal(t, keys, event.S3Keys)
	assert.Greater(t, event.Timestamp, int64(0))
}

func TestRabbitMQPublisher_PublishMultiple(t *testing.T) {
	env := setupTest(t)

	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		require.NoError(t, env.pub.PublishAvatarUploaded(context.Background(), id, "user-1", fmt.Sprintf("key-%d", i)))
	}

	// Порядок публикации сохраняется в очереди.
	for i, id := range ids {
		msg := getMessage(t, env.reader, QueueAvatarUploaded)

		var event AvatarUploadEvent
		require.NoError(t, json.Unmarshal(msg.Body, &event))
		assert.Equal(t, id.String(), event.AvatarID)
		assert.Equal(t, fmt.Sprintf("key-%d", i), event.S3Key)
	}
}

func TestRabbitMQPublisher_Close(t *testing.T) {
	env := setupTest(t)

	assert.NoError(t, env.pub.Close())
	// Повторный Close безопасен.
	assert.NoError(t, env.pub.Close())
}

func TestNewRabbitMQ_InvalidURL(t *testing.T) {
	// Недоступный порт: соединение не установится, конструктор вернёт ошибку.
	_, err := NewRabbitMQ(config.RabbitMQConfig{URL: "amqp://guest:guest@127.0.0.1:1/", Exchange: "x"}, testLogger())
	require.Error(t, err)
}

// TestRabbitMQPublisher_BreakerOpensOnPublishFailure проверяет, что публикация
// обёрнута в breaker: после пяти неудач подряд вызовы не доходят до брокера.
//
// Закрытый канал — самый дешёвый способ получить деградацию брокера без
// остановки контейнера: PublishWithContext сразу возвращает amqp.ErrClosed.
func TestRabbitMQPublisher_BreakerOpensOnPublishFailure(t *testing.T) {
	env := setupTest(t)

	require.NoError(t, env.pub.channel.Close())

	ctx := context.Background()
	avatarID := uuid.New()

	for i := 0; i < breakerFailuresToTrip; i++ {
		err := env.pub.PublishAvatarUploaded(ctx, avatarID, "user-1", "originals/user-1/a/photo.jpg")
		require.Error(t, err, "публикация %d должна упасть", i)
		require.NotErrorIs(t, err, circuitbreaker.ErrCircuitOpen)
	}

	err := env.pub.PublishAvatarUploaded(ctx, avatarID, "user-1", "originals/user-1/a/photo.jpg")
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen)
}

func TestRabbitMQPublisher_DeclareIdempotent(t *testing.T) {
	env := setupTest(t)

	// Повторное объявление той же топологии (тот же exchange) не должно падать.
	second, err := NewRabbitMQ(config.RabbitMQConfig{URL: rabbitURL, Exchange: env.exchange}, testLogger())
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

// TestDeclareTopology_RetryQueueArgs проверяет, что retry-очереди объявлены с
// ожидаемым TTL: повторное объявление с тем же TTL проходит, а с другим —
// падает с PRECONDITION_FAILED (значит, x-message-ttl действительно задан).
func TestDeclareTopology_RetryQueueArgs(t *testing.T) {
	env := setupTest(t)

	require.NoError(t, DeclareTopology(env.reader, testExchange, DefaultRetryTTL))

	err := DeclareTopology(env.reader, testExchange, 5*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PRECONDITION_FAILED")
}
