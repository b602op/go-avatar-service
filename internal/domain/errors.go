package domain

import "errors"

var (
	// ErrNotFound — сущность не найдена в хранилище.
	ErrNotFound = errors.New("domain: not found")

	// ErrAlreadyExists — сущность уже существует.
	ErrAlreadyExists = errors.New("domain: already exists")

	// ErrForbidden — операция запрещена (например, удаление чужого аватара).
	ErrForbidden = errors.New("domain: forbidden")

	// ErrInvalidFormat — файл не прошёл валидацию (MIME-тип или сигнатура).
	ErrInvalidFormat = errors.New("domain: invalid format")

	// ErrFileTooLarge — размер файла превышает допустимый предел.
	ErrFileTooLarge = errors.New("domain: file too large")

	// ErrNotImplemented — операция ещё не реализована (например, ресайз).
	ErrNotImplemented = errors.New("domain: not implemented")
)
