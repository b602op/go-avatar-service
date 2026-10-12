package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b602op/avatars-service/internal/domain"
)

// AvatarRepository — реализация domain.AvatarRepository на PostgreSQL.
type AvatarRepository struct {
	pool *pgxpool.Pool
}

// NewAvatarRepository создаёт репозиторий аватарок.
func NewAvatarRepository(pool *pgxpool.Pool) *AvatarRepository {
	return &AvatarRepository{pool: pool}
}

// Create вставляет запись в таблицу avatars.
func (r *AvatarRepository) Create(ctx context.Context, a *domain.Avatar) error {
	thumbJSON, err := marshalThumbnails(a.ThumbnailS3Keys)
	if err != nil {
		return err
	}

	const q = `
		INSERT INTO avatars (
			id, user_id, file_name, mime_type, size_bytes, s3_key,
			thumbnail_s3_keys, upload_status, processing_status,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`

	_, err = r.pool.Exec(ctx, q,
		a.ID, a.UserID, a.FileName, a.MimeType, a.SizeBytes, a.S3Key,
		thumbJSON, string(a.UploadStatus), string(a.ProcessingStatus),
		a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: create avatar: %w", err)
	}
	return nil
}

// GetByID возвращает аватар по ID.
func (r *AvatarRepository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Avatar, error) {
	const q = `
		SELECT id, user_id, file_name, mime_type, size_bytes, s3_key,
		       thumbnail_s3_keys, upload_status, processing_status,
		       created_at, updated_at, deleted_at
		FROM avatars
		WHERE id = $1 AND deleted_at IS NULL
	`

	row := r.pool.QueryRow(ctx, q, id)
	a, err := scanAvatar(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get avatar: %w", err)
	}
	return a, nil
}

// ListByUserID возвращает все активные аватары пользователя.
func (r *AvatarRepository) ListByUserID(ctx context.Context, userID string) ([]*domain.Avatar, error) {
	const q = `
		SELECT id, user_id, file_name, mime_type, size_bytes, s3_key,
		       thumbnail_s3_keys, upload_status, processing_status,
		       created_at, updated_at, deleted_at
		FROM avatars
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`

	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list avatars: %w", err)
	}
	defer rows.Close()

	var result []*domain.Avatar
	for rows.Next() {
		a, err := scanAvatar(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan avatar: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: rows error: %w", err)
	}
	return result, nil
}

// UpdateProcessingStatus обновляет статус обработки и миниатюры.
func (r *AvatarRepository) UpdateProcessingStatus(
	ctx context.Context,
	id uuid.UUID,
	status domain.ProcessingStatus,
	thumbnailKeys map[string]string,
) error {
	thumbJSON, err := marshalThumbnails(thumbnailKeys)
	if err != nil {
		return err
	}

	const q = `
		UPDATE avatars
		SET processing_status = $1,
		    thumbnail_s3_keys = $2,
		    updated_at = NOW()
		WHERE id = $3 AND deleted_at IS NULL
	`

	tag, err := r.pool.Exec(ctx, q, string(status), thumbJSON, id)
	if err != nil {
		return fmt.Errorf("postgres: update processing status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// UpdateUploadStatus обновляет upload_status.
func (r *AvatarRepository) UpdateUploadStatus(ctx context.Context, id uuid.UUID, status domain.UploadStatus) error {
	const q = `
		UPDATE avatars
		SET upload_status = $1,
		    updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
	`

	tag, err := r.pool.Exec(ctx, q, string(status), id)
	if err != nil {
		return fmt.Errorf("postgres: update upload status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SoftDelete помечает запись удалённой.
func (r *AvatarRepository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE avatars
		SET deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL
	`

	tag, err := r.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("postgres: soft delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ListPending возвращает pending-аватары старше olderThan.
func (r *AvatarRepository) ListPending(ctx context.Context, olderThan time.Duration) ([]*domain.Avatar, error) {
	const q = `
		SELECT id, user_id, file_name, mime_type, size_bytes, s3_key,
		       thumbnail_s3_keys, upload_status, processing_status,
		       created_at, updated_at, deleted_at
		FROM avatars
		WHERE processing_status = 'pending'
		  AND deleted_at IS NULL
		  AND created_at < NOW() - $1::interval
		ORDER BY created_at ASC
	`

	rows, err := r.pool.Query(ctx, q, olderThan.String())
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending: %w", err)
	}
	defer rows.Close()

	var result []*domain.Avatar
	for rows.Next() {
		a, err := scanAvatar(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan pending: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: rows error: %w", err)
	}
	return result, nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

// rowScanner — общий интерфейс для pgx.Row и pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAvatar читает одну строку в domain.Avatar.
func scanAvatar(s rowScanner) (*domain.Avatar, error) {
	var (
		a         domain.Avatar
		thumbRaw  []byte
		uploadSt  string
		processSt string
		deletedAt *time.Time
	)

	err := s.Scan(
		&a.ID, &a.UserID, &a.FileName, &a.MimeType, &a.SizeBytes, &a.S3Key,
		&thumbRaw, &uploadSt, &processSt,
		&a.CreatedAt, &a.UpdatedAt, &deletedAt,
	)
	if err != nil {
		return nil, err
	}

	a.UploadStatus = domain.UploadStatus(uploadSt)
	a.ProcessingStatus = domain.ProcessingStatus(processSt)
	a.DeletedAt = deletedAt

	if len(thumbRaw) > 0 {
		if err := json.Unmarshal(thumbRaw, &a.ThumbnailS3Keys); err != nil {
			return nil, fmt.Errorf("unmarshal thumbnails: %w", err)
		}
	}
	return &a, nil
}

// marshalThumbnails превращает map в JSONB-совместимый []byte.
// nil map → nil (в БД запишется NULL).
func marshalThumbnails(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal thumbnails: %w", err)
	}
	return b, nil
}
