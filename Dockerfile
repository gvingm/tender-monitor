# syntax=docker/dockerfile:1.6
# ---- Build stage ----
FROM golang:1.22-alpine AS build

WORKDIR /src

# Кэшируем слой с зависимостями
COPY go.mod ./
RUN go mod download || true

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/tender-monitor .

# ---- Runtime stage ----
# distroless: ca-certificates и tzdata уже внутри — apk не нужен
# (на сервере Dokploy dl-cdn.alpinelinux.org заблокирован, apk add падает)
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/tender-monitor /app/tender-monitor
COPY reports/ /app/reports/

EXPOSE 8787

# Планировщик запускается в 08:00 МСК
ENV TZ=Europe/Moscow

ENTRYPOINT ["/app/tender-monitor"]
