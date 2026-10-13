-- +goose Up
-- Ширина и высота оригинала. NULL = размеры ещё не вычислены (аватар не обработан).
-- 0 — валидное значение, поэтому маркером «не обработано» служит именно NULL.
ALTER TABLE avatars ADD COLUMN IF NOT EXISTS width  INTEGER;
ALTER TABLE avatars ADD COLUMN IF NOT EXISTS height INTEGER;

-- +goose Down
ALTER TABLE avatars DROP COLUMN IF EXISTS width, DROP COLUMN IF EXISTS height;
