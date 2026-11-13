#!/bin/sh
# 对已启动的本机 iot-dev 跑隔离集成测试。
set -eu
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
set -a
# shellcheck disable=SC1091
. ./.env
set +a
go test -tags integration -count=1 -timeout 240s ./internal/integration
