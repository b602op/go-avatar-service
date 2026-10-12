-- +goose Up
-- +goose StatementBegin
-- Таблица для метаданных аватарок пользователей.
CREATE TABLE IF NOT EXISTS avatars (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            VARCHAR(255) NOT NULL,
    file_name          VARCHAR(255) NOT NULL,
    mime_type          VARCHAR(100) NOT NULL,
    size_bytes         BIGINT       NOT NULL,
    s3_key             VARCHAR(500) NOT NULL,
    thumbnail_s3_keys  JSONB,
    upload_status      VARCHAR(50)  NOT NULL DEFAULT 'uploading',
    processing_status  VARCHAR(50)  NOT NULL DEFAULT 'pending',
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ
);

-- Индекс для выборки активных аватарок пользователя.
CREATE INDEX IF NOT EXISTS idx_avatars_user_id
    ON avatars (user_id)
    WHERE deleted_at IS NULL;

-- Индекс для подметания зависших статусов (cleanup-воркер).
CREATE INDEX IF NOT EXISTS idx_avatars_status
    ON avatars (upload_status, processing_status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_avatars_status;
DROP INDEX IF EXISTS idx_avatars_user_id;
DROP TABLE IF EXISTS avatars;
-- +goose StatementEnd
