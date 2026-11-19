#!/bin/sh
# 在 cs 上启动独立的 MySQL、Redis、EMQX 和进程。不打印口令。
set -eu
cd "$(dirname "$0")/.."
sh scripts/cs-env.sh
sh scripts/render-emqx.sh
# 容器里的 emqx 用户是 1000。root 的 600 文件在 Linux 上会变成 eacces。
chown 1000:1000 deployments/local/emqx.conf deployments/local/vm.args
chmod 400 deployments/local/emqx.conf
# EMQX 会把 vm.args 原样复制到数据目录再追加。源文件只有读权限时，复制结果也不能写。
chmod 600 deployments/local/vm.args
chmod 711 deployments/local
# Docker 新建的数据卷属主是 root，EMQX 以 1000 运行时写不了。
docker volume create eco-device-emqx >/dev/null
docker run --rm --user root --entrypoint chown \
  -v eco-device-emqx:/opt/emqx/data \
  docker.m.daocloud.io/emqx/emqx:6.3.3 \
  -R 1000:1000 /opt/emqx/data
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
