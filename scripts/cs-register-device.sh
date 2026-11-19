#!/bin/sh
# 登记一台 12 位设备号。口令和令牌不打印。已存在时保持成功。
set -eu
cd "$(dirname "$0")/.."
device_id=${1:?device id}
topic_model=${2:-}
printf '%s\n' "$device_id" | grep -Eq '^[0-9a-f]{12}$'
case "$topic_model" in
  ""|1_0pro|2_0_test|watch) ;;
  *) echo "invalid topic model" >&2; exit 1 ;;
esac
if [ ! -f .env ]; then
  echo "missing .env" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
. ./.env
set +a
login=$(curl -fsS -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${IOT_SEED_ADMIN_PASSWORD}\"}" \
  http://127.0.0.1:18092/api/v1/auth/login)
token=${login#*\"token\":\"}
token=${token%%\"*}
if [ -z "$token" ] || [ "$token" = "$login" ]; then
  echo "login failed" >&2
  exit 1
fi
payload="{\"device_id\":\"${device_id}\",\"product_id\":\"collar\"}"
if [ -n "$topic_model" ]; then
  payload="{\"device_id\":\"${device_id}\",\"product_id\":\"collar\",\"topic_model\":\"${topic_model}\"}"
fi
code=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${token}" \
  -H 'Content-Type: application/json' \
  -d "$payload" \
  http://127.0.0.1:18092/api/v1/devices)
case "$code" in
  201) echo "device ${device_id} registered" ;;
  409)
    if [ -z "$topic_model" ]; then
      echo "device ${device_id} registered"
    else
      code=$(curl -sS -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        -d "{\"topic_model\":\"${topic_model}\"}" \
        "http://127.0.0.1:18092/api/v1/devices/${device_id}/topic-model")
      case "$code" in
        200) echo "device ${device_id} topic_model ${topic_model}" ;;
        *) echo "topic model failed: ${code}" >&2; exit 1 ;;
      esac
    fi
    ;;
  *) echo "register failed: ${code}" >&2; exit 1 ;;
esac
