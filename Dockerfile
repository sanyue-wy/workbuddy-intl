# Multi-stage Dockerfile for WorkBuddy International Edition
FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod ./
# 若后续添加依赖，可预先缓存
RUN go mod download 2>/dev/null || true

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=1.0.0" \
    -trimpath \
    -o workbuddy-intl main.go

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -S workbuddy && adduser -S workbuddy -G workbuddy

WORKDIR /app

COPY --from=builder /app/workbuddy-intl /app/workbuddy-intl
COPY config.json /app/config.json

RUN chown -R workbuddy:workbuddy /app

USER workbuddy

EXPOSE 8780

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD curl -fs http://127.0.0.1:8780/health || exit 1

ENTRYPOINT ["/app/workbuddy-intl"]
CMD ["-config", "/app/config.json"]
