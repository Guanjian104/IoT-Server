#!/bin/sh
# 把本机 .env 渲染成 EMQX 只读配置。文件含钩子令牌，不进入仓库。
set -eu
cd "$(dirname "$0")/.."
if [ ! -f .env ]; then
  echo "missing .env" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
. ./.env
set +a
hex='^[0-9a-fA-F]{32,}$'
for value in "$IOT_MQTT_HOOK_TOKEN" "$EMQX_NODE_COOKIE" "$EMQX_DEV_DASHBOARD_PASSWORD"; do
  printf '%s\n' "$value" | grep -Eq "$hex"
done
mkdir -p deployments/local
umask 077
cat > deployments/local/emqx.conf <<EOF
node {
  name = "emqx@127.0.0.1"
  cookie = "${EMQX_NODE_COOKIE}"
  data_dir = "data"
}

cluster {
  name = emqxcl
  discovery_strategy = manual
}

log.console.level = warning

listeners.tcp.default {
  bind = "0.0.0.0:1883"
  enable_authn = true
  max_connections = 10000
}

listeners.ssl.default.enable = false
listeners.ws.default.enable = false
listeners.wss.default.enable = false

dashboard {
  listeners.http.bind = "0.0.0.0:18083"
}

mqtt {
  max_packet_size = "64KB"
  retain_available = true
  shared_subscription = true
  strict_mode = false
}

sys_topics {
  sys_msg_interval = 1m
  sys_heartbeat_interval = 30s
  sys_event_messages {
    client_connected = true
    client_disconnected = true
    client_subscribed = false
    client_unsubscribed = false
  }
}

authentication = [
  {
    mechanism = password_based
    backend = http
    enable = true
    method = post
    url = "http://host.docker.internal:18080/internal/mqtt/auth"
    headers {
      "content-type" = "application/json"
      "x-iot-hook-token" = "${IOT_MQTT_HOOK_TOKEN}"
    }
    body {
      username = "\${username}"
      password = "\${password}"
      clientid = "\${clientid}"
    }
    pool_size = 8
    connect_timeout = 5s
    request_timeout = 5s
  },
  {
    mechanism = cinfo
    enable = true
    checks = [
      {
        is_match = "regex_match(clientid, '.*')"
        result = deny
      }
    ]
  }
]

authorization {
  no_match = deny
  deny_action = disconnect
  sources = [
    {
      type = http
      enable = true
      method = post
      url = "http://host.docker.internal:18080/internal/mqtt/acl"
      headers {
        "content-type" = "application/json"
        "x-iot-hook-token" = "${IOT_MQTT_HOOK_TOKEN}"
      }
      body {
        username = "\${username}"
        clientid = "\${clientid}"
        topic = "\${topic}"
        action = "\${action}"
      }
      pool_size = 8
      connect_timeout = 5s
      request_timeout = 5s
    }
  ]
}
EOF
chmod 600 deployments/local/emqx.conf
cat > deployments/local/vm.args <<'EOF'
+spp true
+A 2
+IOt 2
+stbt db
+sbwt none
+sbwtdcpu none
+sbwtdio none
+C multi_time_warp
+c true
+pc unicode
+S 2:2
EOF
chmod 600 deployments/local/vm.args
