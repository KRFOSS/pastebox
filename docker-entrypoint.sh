#!/bin/sh
set -eu

DATA_DIR="${DATA_DIR:-/paste-data}"
APP_USER="${APP_USER:-pastebox}"
APP_GROUP="${APP_GROUP:-pastebox}"

mkdir -p "$DATA_DIR"

chown -R "$APP_USER:$APP_GROUP" "$DATA_DIR" 2>/dev/null || true
chmod -R u+rwX,g+rwX,o+rwX "$DATA_DIR" 2>/dev/null || true

exec su-exec "$APP_USER:$APP_GROUP" "$@"
