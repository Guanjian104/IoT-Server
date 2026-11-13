# iot-server

本地 IoT 业务进程。未配置 MySQL、Redis、MQTT 时保持空骨架：只提供配置、日志、存活/就绪探针、版本查询和优雅退出。配齐本机依赖后，同一进程提供设备、权限和 MQTT 闭环。RTC 和真实 APP 不在当前范围。

进度见 `docs/实施进度.md`。G1 需要真实设备和真实 APP，当前记录保持未通过。

## 启动

```sh
cd /Users/admin/OtherCode/iot-server
export GOTOOLCHAIN=local
go run ./cmd/server
```

默认监听 `127.0.0.1:8080`。停止进程发送 SIGINT 或 SIGTERM，进程完成退出后返回 0。

指定构建版本：

```sh
VERSION=b02 REVISION=local sh scripts/build.sh
IOT_INSTANCE_ID=local-1 IOT_CONFIG_VERSION=dev ./bin/server
```

## 探针

| 方法 | 路径 | 含义 |
|---|---|---|
| GET, HEAD | `/health/live` | 进程可响应时返回 200 |
| GET, HEAD | `/health/ready` | 正在服务时返回 200；退出过程中返回 503 |
| GET, HEAD | `/version` | `instance_id`、`config_version`、`build_version`、`revision`、`go_version` |

## 配置

环境变量覆盖 `IOT_CONFIG_FILE` 指向的 `KEY=VALUE` 文件。模板见 `configs/config.env.example`。

| 键 | 默认 |
|---|---|
| `IOT_HTTP_ADDR` | `127.0.0.1:8080` |
| `IOT_INSTANCE_ID` | 主机名和进程号 |
| `IOT_CONFIG_VERSION` | `dev` |
| `IOT_LOG_LEVEL` | `info` |
| `IOT_SHUTDOWN_TIMEOUT` | `10s` |
| `IOT_CONFIG_FILE` | 不读取文件 |

启动失败时只输出失败的配置键，不输出配置值。日志会屏蔽键名中含 `password`、`secret`、`token`、`credential` 的值。

## 本机依赖

`scripts/dev-up.sh` 启动名称前缀为 `iot-dev` 的 MySQL、Redis 和 EMQX，只绑定 `127.0.0.1`：MySQL 13306、Redis 16379、MQTT 11883、Dashboard 18083。口令写在 gitignore 的 `.env`，脚本不打印值。环境变量名包括 `IOT_MYSQL_PASSWORD`、`IOT_REDIS_PASSWORD`、`EMQX_DEV_DASHBOARD_PASSWORD`、`EMQX_DEV_APP_PASSWORD`、`IOT_MQTT_DEVICE_PASSWORD`、`IOT_MQTT_HOOK_TOKEN`、`IOT_SEED_ADMIN_PASSWORD`、`IOT_SEED_OPERATOR_PASSWORD`、`IOT_MYSQL_ROOT_PASSWORD`、`EMQX_NODE_COOKIE`。

配齐这些变量后，把 HTTP 指到 `127.0.0.1:18080` 再启动进程。就绪探针在 MySQL、Redis 和 MQTT 都可用时返回 200，任一依赖失败时返回 503。`sh scripts/dev-down.sh` 停止容器并保留数据卷。

```sh
cd /Users/admin/OtherCode/iot-server
export GOTOOLCHAIN=local
sh scripts/dev-up.sh
sh scripts/check.sh
sh scripts/integration.sh
```

`scripts/check.sh` 不连接容器。`scripts/integration.sh` 读取 `.env`，用模拟客户端跑 MQTT 闭环和 RTC 控制平面，并在结束时拉起被它停过的 MySQL 和 Redis。真实设备、真实 APP、真实媒体服务和公网 Broker 不在这条命令里。控制平面的测试凭证标记为不可连接。数据库短时不可用时，进程仍保持 MQTT 连接，并在恢复后按收到顺序重试尚未确认的入站消息。控制平面的请求、响应和错误码见 `docs/控制平面请求集.md`。

## 检查

```sh
sh scripts/check.sh
```

该脚本依次执行 `gofmt -l`、`go test ./...`、`go vet ./...` 和 `go test -race ./...`。仓库使用 Go 1.27.3，并且 `GOTOOLCHAIN=local`，避免自动下载其他工具链。
