# syntax=docker/dockerfile:1
FROM alpine:3.24 AS builder

RUN printf '%s\n' \
  'https://mirror5.krfoss.org/alpine/v3.24/main' \
  'https://mirror5.krfoss.org/alpine/v3.24/community' \
  > /etc/apk/repositories \
  && apk update \
  && apk upgrade --no-cache \
  && apk add --no-cache go ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/pastebox ./cmd/server

FROM alpine:3.24

RUN printf '%s\n' \
  'https://mirror5.krfoss.org/alpine/v3.24/main' \
  'https://mirror5.krfoss.org/alpine/v3.24/community' \
  > /etc/apk/repositories \
  && apk update \
  && apk upgrade --no-cache \
  && apk add --no-cache ca-certificates tzdata su-exec wget \
  && addgroup -S pastebox \
  && adduser -S -G pastebox -h /app pastebox \
  && mkdir -p /paste-data \
  && chown -R pastebox:pastebox /paste-data /app

WORKDIR /app
COPY --from=builder /out/pastebox /app/pastebox

COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1
ENV DATA_DIR=/paste-data \
    LISTEN_ADDR=:8080
VOLUME ["/paste-data"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/pastebox"]
