#!/bin/sh
# 把本机 .env 渲染成 EMQX 只读配置。文件含节点 cookie 和只读库口令，不进入仓库。
# root 与 eco-iot 的口令在 eco_iot.mqtt_credential。能不能连、能碰哪些主题，都查这只库。
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
for value in "$EMQX_NODE_COOKIE" "$EMQX_DEV_DASHBOARD_PASSWORD" "${IOT_EMQX_DEVICE_DB_PASSWORD:-}"; do
  printf '%s\n' "$value" | grep -Eq "$hex" || {
    echo "invalid EMQX secret" >&2
    exit 1
  }
done
mkdir -p deployments/local
umask 077
# 认证只走 MySQL。eco-iot 按用户名对口令。
# root 还要看客户端号：已登记的 1_0pro、2_0_test，或观察连接 9030d6cb0305。
# 查不到行就拒绝，不再退回内置账号表。
# 允许规则里，设备自己的 state/cmd 仍按 device 现查；eco-iot 的旧主题和观察连接在 mqtt_acl。
# 查询里的 ${clientid}、${username} 由 EMQX 在执行时替换，这里只保留一个 $。
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

authentication_settings {
  node_cache {
    enable = false
  }
}

authentication = [
  {
    mechanism = password_based
    backend = mysql
    enable = true
    server = "eco-mysql:3306"
    database = "eco_iot"
    username = "emqx_gate"
    password = "${IOT_EMQX_DEVICE_DB_PASSWORD}"
    pool_size = 2
    query_timeout = "5s"
    password_hash_algorithm {
      name = sha256
      salt_position = suffix
    }
    query = "SELECT c.password_hash, c.salt FROM mqtt_credential AS c INNER JOIN (SELECT \${username} AS username, \${clientid} AS clientid) AS gate ON c.username = gate.username WHERE c.username = 'eco-iot' OR (c.username = 'root' AND (gate.clientid = '9030d6cb0305' OR EXISTS (SELECT 1 FROM device AS d WHERE d.sn = gate.clientid AND d.topic_model IN ('1_0pro', '2_0_test'))))"
  }
]

authorization {
  no_match = deny
  deny_action = ignore
  cache {
    enable = false
  }
  node_cache {
    enable = false
  }
  sources = [
    {
      type = mysql
      enable = true
      server = "eco-mysql:3306"
      database = "eco_iot"
      username = "emqx_gate"
      password = "${IOT_EMQX_DEVICE_DB_PASSWORD}"
      pool_size = 2
      query_timeout = "5s"
      query = "SELECT 'allow' AS permission, 'publish' AS action, CONCAT('pet/collar/', d.topic_model, '/', d.sn, '/state') AS topic FROM device AS d INNER JOIN (SELECT \${username} AS username, \${clientid} AS clientid) AS gate ON d.sn = gate.clientid WHERE d.topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT 'allow', 'subscribe', CONCAT('pet/collar/', d.topic_model, '/', d.sn, '/cmd') FROM device AS d INNER JOIN (SELECT \${username} AS username, \${clientid} AS clientid) AS gate ON d.sn = gate.clientid WHERE d.topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT 'allow', 'publish', CONCAT('pet/collar/', d.topic_model, '/', d.sn, '/cmd') FROM device AS d INNER JOIN (SELECT \${username} AS username, \${clientid} AS clientid) AS gate ON gate.username = 'eco-iot' WHERE d.topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT a.permission, a.action, a.topic FROM mqtt_acl AS a INNER JOIN (SELECT \${username} AS username, \${clientid} AS clientid) AS gate ON a.username = gate.username AND (a.clientid = '' OR a.clientid = gate.clientid)"
    }
  ]
}
EOF
chmod 600 deployments/local/emqx.conf
if [ "$(id -u)" -eq 0 ]; then
  chown 1000:1000 deployments/local/emqx.conf deployments/local/vm.args
fi
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
