#!/bin/sh
# 停止 iot-dev 容器，保留数据卷。
set -eu
cd "$(dirname "$0")/.."
docker compose --env-file .env -p iot-dev -f deployments/compose.yaml stop
