# syntax=docker/dockerfile:1

# Stage 1: Static zero-CGO binary build
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.4.4
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/azrinaldi/gemini-stealth-bridge/internal/config.Version=${VERSION}" \
    -o /out/gsb ./cmd/gsb

# Stage 2: Minimal non-root runtime (~12 MB image, ~5 MB RSS idle)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /data gsb \
    && mkdir -p /data/accounts \
    && chown -R gsb:gsb /data

USER gsb
WORKDIR /data

ENV BRIDGE_HOME=/data \
    BRIDGE_HOST=0.0.0.0 \
    BRIDGE_PORT=20133 \
    BRIDGE_WEB_UI=1

VOLUME ["/data"]
EXPOSE 20133

COPY --from=builder /out/gsb /usr/local/bin/gsb

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/gsb", "--healthcheck"]

ENTRYPOINT ["/usr/local/bin/gsb"]
