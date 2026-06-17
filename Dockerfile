# syntax=docker/dockerfile:1
FROM alpine:3.23 AS builder

RUN printf '%s\n' \
  'https://mirror5.krfoss.org/alpine/v3.23/main' \
  'https://mirror5.krfoss.org/alpine/v3.23/community' \
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

FROM alpine:3.23

RUN printf '%s\n' \
  'https://mirror5.krfoss.org/alpine/v3.23/main' \
  'https://mirror5.krfoss.org/alpine/v3.23/community' \
  > /etc/apk/repositories \
  && apk update \
  && apk upgrade --no-cache \
  && apk add --no-cache ca-certificates tzdata su-exec \
  && addgroup -S pastebox \
  && adduser -S -G pastebox -h /app pastebox \
  && mkdir -p /paste-data /app/templates \
  && chown -R pastebox:pastebox /paste-data /app

WORKDIR /app
COPY --from=builder /out/pastebox /app/pastebox
COPY templates ./templates
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 8080
ENV DATA_DIR=/paste-data \
    LISTEN_ADDR=:8080
VOLUME ["/paste-data"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/pastebox"]
