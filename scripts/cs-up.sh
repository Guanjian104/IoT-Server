#!/bin/sh
# 在 cs 上启动独立的 MySQL、Redis、EMQX 和进程。不打印口令。
set -eu
cd "$(dirname "$0")/.."
sh scripts/cs-env.sh
sh scripts/render-emqx.sh
export IOT_BUILD_VERSION=cs
export IOT_BUILD_REVISION=$(git -c safe.directory="$PWD" rev-parse --short HEAD)
docker compose --env-file .env -p eco-device -f deployments/compose.cs.yaml up -d --build
i=0
while [ "$i" -lt 90 ]; do
  if curl -fsS http://127.0.0.1:18092/health/ready | grep -q '"status":"ready"'; then
    echo "eco-device ready"
    exit 0
  fi
  i=$((i + 1))
  sleep 2
done
echo "eco-device did not become ready" >&2
docker ps -a --filter name=eco-device --format '{{.Names}} {{.Status}}' >&2
exit 1
