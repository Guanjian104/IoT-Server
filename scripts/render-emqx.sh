#!/bin/sh
# 把本机 .env 渲染成 EMQX 只读配置。文件含节点 cookie 和只读库口令，不进入仓库。
# eco-iot 与 root 的口令仍在内置库。项圈能不能连，查 eco_iot.device，不回调业务进程。
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
# root 先走 MySQL：设备不在表里时返回一行对不上的散列，连接在内置库之前被拒绝。
# 查到 sn 且 topic_model 为 1_0pro 或 2_0_test 时不返回行，接着用内置库核对 root 口令。
# 观察连接 9030d6cb0305 同样不返回行，口令仍走内置库。它只能订 state、向具体代次的 cmd 发布。
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
    precondition = "str_eq(username, 'root')"
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
    query = "SELECT '0000000000000000000000000000000000000000000000000000000000000000' AS password_hash, '' AS salt FROM (SELECT \${clientid} AS cid) AS gate WHERE gate.cid <> '9030d6cb0305' AND NOT EXISTS (SELECT 1 FROM device WHERE sn = gate.cid AND topic_model IN ('1_0pro', '2_0_test'))"
  },
  {
    mechanism = password_based
    backend = built_in_database
    user_id_type = username
    password_hash_algorithm {
      name = sha256
      salt_position = suffix
    }
    enable = true
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
      type = built_in_database
      enable = true
    },
    {
      type = mysql
      enable = true
      server = "eco-mysql:3306"
      database = "eco_iot"
      username = "emqx_gate"
      password = "${IOT_EMQX_DEVICE_DB_PASSWORD}"
      pool_size = 2
      query_timeout = "5s"
      query = "SELECT 'allow' AS permission, 'publish' AS action, CONCAT('pet/collar/', topic_model, '/', sn, '/state') AS topic FROM device WHERE sn = \${clientid} AND topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT 'allow', 'subscribe', CONCAT('pet/collar/', topic_model, '/', sn, '/cmd') FROM device WHERE sn = \${clientid} AND topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT 'allow', 'publish', CONCAT('pet/collar/', topic_model, '/', sn, '/cmd') FROM device WHERE \${username} = 'eco-iot' AND topic_model IN ('1_0pro', '2_0_test') UNION ALL SELECT 'allow', 'subscribe', 'pet/collar/+/+/state' WHERE \${username} = 'root' AND \${clientid} = '9030d6cb0305' UNION ALL SELECT 'allow', 'publish', 'pet/collar/1_0pro/+/cmd' WHERE \${username} = 'root' AND \${clientid} = '9030d6cb0305' UNION ALL SELECT 'allow', 'publish', 'pet/collar/2_0_test/+/cmd' WHERE \${username} = 'root' AND \${clientid} = '9030d6cb0305'"
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
