#!/bin/sh
# 启动已授权的 iot-dev 容器。不删除数据卷，不打印口令。
set -eu
cd "$(dirname "$0")/.."
sh scripts/dev-env.sh
sh scripts/render-emqx.sh
docker compose --env-file .env -p iot-dev -f deployments/compose.yaml up -d
i=0
while [ "$i" -lt 60 ]; do
  mysql=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' iot-dev-mysql)
  redis=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' iot-dev-redis)
  emqx=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' iot-dev-emqx)
  if [ "$mysql" = healthy ] && [ "$redis" = healthy ] && [ "$emqx" = healthy ]; then
    echo "iot-dev healthy"
    exit 0
  fi
  i=$((i + 1))
  sleep 2
done
echo "iot-dev did not become healthy" >&2
docker ps -a --filter name=iot-dev --format '{{.Names}} {{.Status}}' >&2
exit 1
