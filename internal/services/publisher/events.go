package publisher

// AvatarUploadEvent — событие о загрузке оригинала аватарки.
//
// Публикуется в routing key "avatar.uploaded"; воркер использует его для
// создания миниатюр.
type AvatarUploadEvent struct {
	AvatarID  string `json:"avatar_id"`
	UserID    string `json:"user_id"`
	S3Key     string `json:"s3_key"`
	Timestamp int64  `json:"timestamp"`
}

// AvatarDeleteEvent — событие об удалении аватарки.
//
// Публикуется в routing key "avatar.deleted"; воркер удаляет перечисленные
// объекты (оригинал и миниатюры) из S3.
type AvatarDeleteEvent struct {
	AvatarID  string   `json:"avatar_id"`
	UserID    string   `json:"user_id"`
	S3Keys    []string `json:"s3_keys"`
	Timestamp int64    `json:"timestamp"`
}
