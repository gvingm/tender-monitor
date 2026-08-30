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
FROM alpine:3.20

RUN apk add --no-cache ca-certificates curl \
    && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/tender-monitor /app/tender-monitor

USER app
EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:8787/ || exit 1

ENTRYPOINT ["/app/tender-monitor"]
