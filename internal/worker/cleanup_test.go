package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b602op/avatars-service/internal/domain"
)

// stubPublisher — ручной стаб EventPublisher, считает опубликованные события.
type stubPublisher struct {
	uploadedIDs   []uuid.UUID
	uploadedCount int
	// uploadedFn позволяет задать ошибку публикации или проверить состояние
	// репозитория в момент публикации.
	uploadedFn func(ctx context.Context, avatarID uuid.UUID) error
}

func (s *stubPublisher) PublishAvatarUploaded(ctx context.Context, avatarID uuid.UUID, _, _ string) error {
	s.uploadedIDs = append(s.uploadedIDs, avatarID)
	s.uploadedCount++
	if s.uploadedFn != nil {
		return s.uploadedFn(ctx, avatarID)
	}
	return nil
}

func (s *stubPublisher) PublishAvatarDeleted(context.Context, uuid.UUID, string, []string) error {
	return nil
}

// Проверка на этапе компиляции, что стаб реализует доменный интерфейс.
var _ domain.EventPublisher = (*stubPublisher)(nil)

// pendingAvatar — тестовый аватар в статусе pending.
func pendingAvatar() *domain.Avatar {
	return &domain.Avatar{
		ID:     uuid.New(),
		UserID: "user-1",
		S3Key:  "originals/user-1/" + uuid.NewString() + "/photo.jpg",
	}
}

func TestCleanup_RepublishesPending(t *testing.T) {
	avatars := []*domain.Avatar{pendingAvatar(), pendingAvatar(), pendingAvatar()}
	repo := &stubRepo{
		listPendingFn: func(_ context.Context, olderThan time.Duration) ([]*domain.Avatar, error) {
			assert.Equal(t, 2*time.Minute, olderThan)
			return avatars, nil
		},
	}
	pub := &stubPublisher{}
	// В момент публикации запись уже должна быть помечена processing — иначе
	// ListPending вернул бы её повторно и переопубликация зациклилась бы.
	pub.uploadedFn = func(_ context.Context, avatarID uuid.UUID) error {
		assert.Equal(t, []domain.ProcessingStatus{domain.ProcessingStatusProcessing}, repo.statusesFor(avatarID))
		return nil
	}
	c := NewCleanup(repo, pub, testLogger(), time.Minute, 2*time.Minute)

	c.runOnce(context.Background())

	require.Equal(t, 3, pub.uploadedCount)
	assert.ElementsMatch(t, []uuid.UUID{avatars[0].ID, avatars[1].ID, avatars[2].ID}, pub.uploadedIDs)

	// Каждая запись помечена processing ровно один раз.
	require.Len(t, repo.procUpdates, 3)
	for _, a := range avatars {
		assert.Equal(t, []domain.ProcessingStatus{domain.ProcessingStatusProcessing}, repo.statusesFor(a.ID))
	}
}

func TestCleanup_PublishFails_RollsBackToPending(t *testing.T) {
	avatar := pendingAvatar()
	repo := &stubRepo{
		listPendingFn: func(context.Context, time.Duration) ([]*domain.Avatar, error) {
			return []*domain.Avatar{avatar}, nil
		},
	}
	pub := &stubPublisher{
		uploadedFn: func(context.Context, uuid.UUID) error {
			return assert.AnError
		},
	}
	c := NewCleanup(repo, pub, testLogger(), time.Minute, 2*time.Minute)

	c.runOnce(context.Background())

	require.Equal(t, 1, pub.uploadedCount)
	// Сначала processing, затем откат в pending — чтобы попробовать на следующем круге.
	assert.Equal(t, []domain.ProcessingStatus{
		domain.ProcessingStatusProcessing,
		domain.ProcessingStatusPending,
	}, repo.statusesFor(avatar.ID))
}

func TestCleanup_MarkProcessingFails_SkipsPublish(t *testing.T) {
	avatar := pendingAvatar()
	repo := &stubRepo{
		listPendingFn: func(context.Context, time.Duration) ([]*domain.Avatar, error) {
			return []*domain.Avatar{avatar}, nil
		},
		updateProcFn: func(context.Context, uuid.UUID, domain.ProcessingStatus) error {
			return assert.AnError
		},
	}
	pub := &stubPublisher{}
	c := NewCleanup(repo, pub, testLogger(), time.Minute, 2*time.Minute)

	c.runOnce(context.Background())

	// Не пометили — не публикуем: иначе получили бы дубль события.
	assert.Zero(t, pub.uploadedCount)
	require.Len(t, repo.procUpdates, 1)
	assert.Equal(t, domain.ProcessingStatusProcessing, repo.procUpdates[0].status)
}

func TestCleanup_NoPending(t *testing.T) {
	repo := &stubRepo{
		listPendingFn: func(context.Context, time.Duration) ([]*domain.Avatar, error) {
			return nil, nil
		},
	}
	pub := &stubPublisher{}
	c := NewCleanup(repo, pub, testLogger(), time.Minute, 2*time.Minute)

	c.runOnce(context.Background())

	assert.Zero(t, pub.uploadedCount)
}

// TestCleanup_Run_StopsOnContext проверяет, что Run завершается при отмене ctx.
func TestCleanup_Run_StopsOnContext(t *testing.T) {
	repo := &stubRepo{}
	c := NewCleanup(repo, &stubPublisher{}, testLogger(), time.Minute, 2*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup.Run не остановился после отмены контекста")
	}
}
