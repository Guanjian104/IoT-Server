#!/bin/sh
# 生成只在本机使用的开发口令。已有 .env 时不覆盖。
set -eu
cd "$(dirname "$0")/.."
umask 077
if [ -f .env ]; then
  exit 0
fi
hex() { openssl rand -hex 24; }
admin=$(hex)
operator=$(hex)
mysql_root=$(hex)
mysql_app=$(hex)
redis=$(hex)
dashboard=$(hex)
app=$(hex)
device=$(hex)
hook=$(hex)
cookie=$(hex)
cat > .env <<EOF
IOT_HTTP_ADDR=127.0.0.1:18080
IOT_HOOK_ADDR=127.0.0.1:18082
IOT_INSTANCE_ID=iot-dev-1
IOT_CONFIG_VERSION=p1
IOT_LOG_LEVEL=info
IOT_SHUTDOWN_TIMEOUT=10s
IOT_TENANT_ID=dev
IOT_COMMAND_DEADLINE=5s
IOT_MYSQL_ADDR=127.0.0.1:13306
IOT_MYSQL_USER=iot_app
IOT_MYSQL_PASSWORD=${mysql_app}
IOT_MYSQL_DATABASE=iot_dev
IOT_MYSQL_ROOT_PASSWORD=${mysql_root}
IOT_REDIS_ADDR=127.0.0.1:16379
IOT_REDIS_PASSWORD=${redis}
IOT_MQTT_BROKER_URL=tcp://127.0.0.1:11883
IOT_MQTT_SERVICE_USERNAME=iot-dev-service
IOT_MQTT_SERVICE_PASSWORD=${app}
IOT_MQTT_DEVICE_USERNAME=root
IOT_MQTT_DEVICE_PASSWORD=${device}
IOT_MQTT_HOOK_TOKEN=${hook}
IOT_EMQX_API_URL=http://127.0.0.1:18083
IOT_EMQX_API_USER=admin
IOT_EMQX_API_PASSWORD=${dashboard}
IOT_SEED_ADMIN_PASSWORD=${admin}
IOT_SEED_OPERATOR_PASSWORD=${operator}
EMQX_DEV_DASHBOARD_PASSWORD=${dashboard}
EMQX_DEV_APP_PASSWORD=${app}
EMQX_NODE_COOKIE=${cookie}
EOF
chmod 600 .env
