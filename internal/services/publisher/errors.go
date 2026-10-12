package publisher

// PermanentError помечает ошибку, повтор которой бессмысленен.
//
// Примеры: невалидный JSON в сообщении, непарсящийся UUID. Воркер отправляет
// такие сообщения сразу в DLQ, не пытаясь обработать их повторно. Ошибки,
// которые НЕ обёрнуты в PermanentError (S3, БД, сеть), считаются transient и
// переотправляются в очередь.
type PermanentError struct {
	Err error
}

// Error возвращает текст вложенной ошибки.
func (e *PermanentError) Error() string {
	if e == nil || e.Err == nil {
		return "publisher: permanent error"
	}
	return e.Err.Error()
}

// Unwrap даёт доступ к вложенной ошибке для errors.Is / errors.As.
func (e *PermanentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
