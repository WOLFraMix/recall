# --- Ступень 1: сборка ---
# Отдельный образ с тулчейном Go: сюда попадает весь кэш модулей
# и компилятора — в финальный образ это не утечёт.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Сначала копируем только манипуляторы зависимостей и качаем модули:
# этот слой кешируется, пока go.mod/go.sum не меняются — пересборка
# после правки кода занимает секунды, а не минуты.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 — полностью статический бинарник, без libc.
# -ldflags="-s -w" — срезаем отладочные символы, exe меньше на ~30%.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /recall ./cmd/recall

# --- Ступень 2: рантайм ---
FROM alpine:3.20
# Не root: даже если контейнер скомпрометируют, процесс не имеет
# прав суперпользователя — базовая гигиена для любого образа.
RUN adduser -D -u 10001 recall
USER recall
WORKDIR /app
COPY --from=build /recall ./recall
# Healthcheck: compose и оркестраторы видят жив ли процесс,
# а не только «порт открыт».
HEALTHCHECK --interval=30s --timeout=3s \
    CMD wget -qO- http://localhost:8080/api/stats || exit 1
EXPOSE 8080
ENTRYPOINT ["./recall"]
