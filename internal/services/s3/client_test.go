package s3

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/domain"
	"github.com/b602op/avatars-service/internal/services/circuitbreaker"
)

// breakerFailuresToTrip — сколько неудач подряд открывают breaker клиента.
// Держим синхронно с circuitbreaker.failuresToTrip (константа не экспортируется).
const breakerFailuresToTrip = 5

// testBucket возвращает имя тестового бакета (отдельно от рабочего).
func testBucket() string {
	if b := os.Getenv("TEST_S3_BUCKET"); b != "" {
		return b
	}
	return "avatars-test"
}

// testConfig собирает конфиг S3 из окружения с дефолтами для локального Silo.
func testConfig() config.S3Config {
	return config.S3Config{
		Endpoint:  envOr("S3_ENDPOINT", "localhost:9000"),
		AccessKey: envOr("S3_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("S3_SECRET_KEY", "minioadmin"),
		Bucket:    testBucket(),
		Region:    envOr("S3_REGION", "us-east-1"),
		UseSSL:    false,
	}
}

// envOr возвращает значение переменной окружения или дефолт.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// setupTest создаёт клиент, гарантирует наличие тестового бакета и чистит его.
func setupTest(t *testing.T) (*Client, context.Context) {
	t.Helper()

	ctx := context.Background()
	client, err := New(testConfig(), nil)
	require.NoError(t, err, "не удалось создать S3-клиент (запусти 'make up')")

	require.NoError(t, client.EnsureBucket(ctx), "не удалось создать тестовый бакет")

	require.NoError(t, clearBucket(ctx, client))

	t.Cleanup(func() {
		_ = clearBucket(ctx, client)
	})

	return client, ctx
}

// clearBucket удаляет все объекты из бакета.
func clearBucket(ctx context.Context, c *Client) error {
	var keys []string
	for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return c.DeleteMany(ctx, keys)
}

func TestClient_EnsureBucket_Idempotent(t *testing.T) {
	client, ctx := setupTest(t)

	require.NoError(t, client.EnsureBucket(ctx))
	require.NoError(t, client.EnsureBucket(ctx))
}

func TestClient_UploadAndDownload(t *testing.T) {
	client, ctx := setupTest(t)

	content := []byte("hello avatar")
	require.NoError(t, client.Upload(ctx, "test/hello.txt", bytes.NewReader(content), int64(len(content)), "text/plain"))

	rc, info, err := client.Download(ctx, "test/hello.txt")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, int64(len(content)), info.Size)
	assert.Equal(t, "text/plain", info.ContentType)
}

func TestClient_Upload_RequiresExactSize(t *testing.T) {
	client, ctx := setupTest(t)

	err := client.Upload(ctx, "test/unknown.txt", bytes.NewReader([]byte("x")), -1, "")
	require.Error(t, err)
}

func TestClient_StatObject_NotFound(t *testing.T) {
	client, ctx := setupTest(t)

	_, err := client.StatObject(ctx, "no/such/key")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestClient_Download_NotFound(t *testing.T) {
	client, ctx := setupTest(t)

	rc, _, err := client.Download(ctx, "no/such/key")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Nil(t, rc)
}

func TestClient_Delete(t *testing.T) {
	client, ctx := setupTest(t)

	content := []byte("to delete")
	require.NoError(t, client.Upload(ctx, "test/del.txt", bytes.NewReader(content), int64(len(content)), ""))
	require.NoError(t, client.Delete(ctx, "test/del.txt"))

	_, err := client.StatObject(ctx, "test/del.txt")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestClient_Delete_NotExists(t *testing.T) {
	client, ctx := setupTest(t)

	// Удаление несуществующего ключа не является ошибкой.
	assert.NoError(t, client.Delete(ctx, "no/such/key"))
}

func TestClient_DeleteMany(t *testing.T) {
	client, ctx := setupTest(t)

	keys := []string{"test/a.txt", "test/b.txt", "test/c.txt"}
	for _, k := range keys {
		content := []byte(k)
		require.NoError(t, client.Upload(ctx, k, bytes.NewReader(content), int64(len(content)), ""))
	}

	require.NoError(t, client.DeleteMany(ctx, keys[:2]))

	// Третий объект остался.
	_, err := client.StatObject(ctx, keys[2])
	require.NoError(t, err)

	// Первые два удалены.
	_, err = client.StatObject(ctx, keys[0])
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = client.StatObject(ctx, keys[1])
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestClient_HealthCheck(t *testing.T) {
	client, ctx := setupTest(t)

	assert.NoError(t, client.HealthCheck(ctx))

	// Клиент с несуществующим бакетом — health-проверка падает.
	cfg := testConfig()
	cfg.Bucket = "definitely-missing-bucket-xyz"
	missing, err := New(cfg, nil)
	require.NoError(t, err)
	assert.Error(t, missing.HealthCheck(ctx))
}

// deadEndpointClient — клиент на закрытом порту: хранилище гарантированно
// недоступно, поэтому для проверки breaker'а не нужен ни compose, ни testcontainers.
func deadEndpointClient(t *testing.T) *Client {
	t.Helper()

	cfg := testConfig()
	// Порт 1 на loopback закрыт: соединение отклоняется сразу, без ожидания.
	cfg.Endpoint = "127.0.0.1:1"

	client, err := New(cfg, nil)
	require.NoError(t, err)
	return client
}

// deadCtx ограничивает вызов по времени: у minio-go есть внутренние ретраи, и
// без дедлайна тест ждал бы их backoff.
func deadCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// Проверяет, что реальные операции клиента обёрнуты в breaker: после пяти
// неудач подряд вызовы перестают уходить в хранилище.
func TestClient_CircuitBreaker_OpensOnStorageFailure(t *testing.T) {
	client := deadEndpointClient(t)
	ctx := deadCtx(t)

	for i := 0; i < breakerFailuresToTrip; i++ {
		_, err := client.StatObject(ctx, "no/such/key")
		require.Error(t, err, "вызов %d должен дойти до хранилища и упасть", i)
		require.NotErrorIs(t, err, circuitbreaker.ErrCircuitOpen)
	}

	_, err := client.StatObject(ctx, "no/such/key")
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen)
}

func TestClient_CircuitBreaker_CoversAllOperations(t *testing.T) {
	client := deadEndpointClient(t)
	ctx := deadCtx(t)

	// Открываем breaker одной операцией, затем проверяем остальные: состояние
	// общее на клиента, поэтому все они должны коротко замкнуться.
	for i := 0; i < breakerFailuresToTrip; i++ {
		_, err := client.StatObject(ctx, "no/such/key")
		require.Error(t, err)
	}

	content := []byte("x")

	err := client.Upload(ctx, "k", bytes.NewReader(content), int64(len(content)), "")
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "Upload")

	_, _, err = client.Download(ctx, "k")
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "Download")

	err = client.Delete(ctx, "k")
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "Delete")

	err = client.DeleteMany(ctx, []string{"k"})
	assert.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "DeleteMany")
}

// EnsureBucket и HealthCheck намеренно вне breaker'а: они должны честно
// показывать состояние хранилища, а не «breaker открыт».
func TestClient_HealthCheck_NotBehindBreaker(t *testing.T) {
	client := deadEndpointClient(t)
	ctx := deadCtx(t)

	for i := 0; i < breakerFailuresToTrip+1; i++ {
		_, err := client.StatObject(ctx, "no/such/key")
		require.Error(t, err)
	}

	// Breaker открыт, но health-проверка всё равно идёт в хранилище и падает
	// своей ошибкой, а не ErrCircuitOpen.
	err := client.HealthCheck(ctx)
	require.Error(t, err)
	assert.NotErrorIs(t, err, circuitbreaker.ErrCircuitOpen)
}
