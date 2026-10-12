package domain

import "errors"

var (
	// ErrNotFound — сущность не найдена в хранилище.
	ErrNotFound = errors.New("domain: not found")

	// ErrAlreadyExists — сущность уже существует.
	ErrAlreadyExists = errors.New("domain: already exists")

	// ErrForbidden — операция запрещена (например, удаление чужого аватара).
	ErrForbidden = errors.New("domain: forbidden")
)
