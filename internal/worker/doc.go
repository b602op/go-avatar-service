// Package worker содержит обработчики событий из брокера сообщений.
//
// Handler выполняет фоновую обработку аватарок: создание миниатюр по событию
// avatar.uploaded и очистку файлов в S3 по событию avatar.deleted. Consumer
// читает очередь RabbitMQ и подтверждает/отклоняет сообщения, Worker
// оркестрирует оба consumer'а и обеспечивает graceful shutdown.
package worker
