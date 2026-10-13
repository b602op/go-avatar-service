# syntax=docker/dockerfile:1

# ─── Build stage ────────────────────────────────────────────────────────────
# Один builder на оба бинарника: сервер и воркер собираются за один проход,
# слои с зависимостями кэшируются и переиспользуются обоими target'ами.
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Зависимости отдельным слоем: он пересобирается только при изменении
# go.mod/go.sum, а не при каждой правке исходников.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 — статические бинарники без glibc, чтобы запускаться в alpine.
# -trimpath убирает абсолютные пути сборки из бинарника, -s -w — отладочные
# символы и таблицу символов: образ меньше, наружу не утекают пути разработчика.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ─── Server runtime ─────────────────────────────────────────────────────────
FROM alpine:3.20 AS server

# ca-certificates — TLS-корни для внешних сервисов, tzdata — корректные зоны
# в логах. app — непривилегированный пользователь (запуск не от root).
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /out/server ./server

# Веб-интерфейс: сервер раздаёт его из web/static относительно рабочей директории.
COPY --from=builder /app/web ./web

USER app

EXPOSE 8080

ENTRYPOINT ["./server"]

# ─── Worker runtime ─────────────────────────────────────────────────────────
FROM alpine:3.20 AS worker

RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /out/worker ./worker

USER app

ENTRYPOINT ["./worker"]
