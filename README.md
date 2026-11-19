# ECO-Device-Server

本地 IoT 进程，Go 模块名是 `iot-server`。它服务项圈 1.0pro：设备登记、权限、MQTT 命令和状态，以及观看和通话的控制平面。配上 `IOT_SFU_ADDR` 时，同一进程在本机转发音视频。

仓库使用 Go 1.27.3，并且 `GOTOOLCHAIN=local`。MQTT 主题和报文见 `docs/MQTT_IOT_PROTOCOL.md`。控制平面的请求体和错误码见 `docs/控制平面请求集.md`。

## 两种启动形态

只给出进程配置时，进程提供配置加载、日志、存活和就绪探针、版本查询、优雅退出。此时不挂业务接口。若同时设置了 `IOT_SFU_ADDR`，本机 WHIP 和 WHEP 会监听，业务房间仍要等存储起来之后才会创建。

配上 MySQL 后，进程在就绪前做迁移和种子数据，并挂上业务接口。再配上 Redis 后，登录和发令才有限流。配上 MQTT 后，进程建立服务端订阅。配上 `IOT_HOOK_ADDR` 后，EMQX 的认证和 ACL 回调才有监听可接。已启用的 MySQL、Redis、MQTT 都要可用，就绪探针才返回 200。`scripts/dev-env.sh` 生成的 `.env` 一次带上这三项，并带上钩子和 SFU 地址。

收到 SIGINT 或 SIGTERM 后，就绪探针先变为 503，HTTP 收完在途请求再退出，成功时进程返回 0。

## 设备与 MQTT

租户固定为 `dev`。登录用用户名和口令，口令以 bcrypt 保存。访问令牌由服务端签发，有效期 12 小时，库里只存摘要。之后的业务请求带 `Authorization: Bearer <token>`。请求体里的用户标识不作为身份。

平台管理员可以登记设备、改授权、启用、禁用和吊销。普通用户按设备持有 `manage`、`view`、`call`、`ticket`。`manage` 覆盖读取和发令；`view` 可以读设备和会话。登录每用户名每分钟 20 次，发令每用户每分钟 30 次，计数在 Redis。Redis 失败时这两类请求返回 503。

设备编号是 12 位小写十六进制，取项圈 MAC。私有服主题按设备登记的代次选择：`1_0pro` 使用 `pet/collar/1_0pro/{mac}/cmd` 和 `pet/collar/1_0pro/{mac}/state`，`2_0_test` 把路径中的代次换成 `2_0_test`。未登记代次时按 `1_0pro`。下发命令没有 `msgId`，命令名是 `find_light`、`buzzer`、`gps_oneshot`、`find_mode`、`trip_track`、`activate`、`mic_record`、`speaker_play`、`voice_call`。设备在 `state` 上回复 `cmd_result`，并上报 `location`、`mode_state`、DP，以及纯文本 `online` / `offline`。命令先入库再发布，状态有 `pending`、`sending`、`sent`、`accepted`、`failed`、`timeout`、`uncertain`、`needs_attention`、`cancelled`。`accepted` 表示收到 `accepted`、`success` 或 `ended`。`failed` 和 `busy` 收成 `failed`。`uncertain` 和 `needs_attention` 可以人工重试或关闭；关闭收成 `failed`。

设备 MQTT 使用共享用户名 `root`，客户端 ID 等于设备编号，口令来自 `IOT_MQTT_DEVICE_PASSWORD`。服务账号用户名固定为 `iot-dev-service`。用户名以 `app:` 开头的连接会被拒绝。

| 角色 | 允许的主题 |
|---|---|
| 设备 | 发布自己这一代次的 `pet/collar/{代次}/{mac}/state`；订阅自己这一代次的 `pet/collar/{代次}/{mac}/cmd` |
| 服务 | 发布对应代次的 `cmd`；共享订阅两个代次的 `state`，并订阅 `$SYS` 连接事件 |

钩子在独立监听上校验认证和 ACL。禁用或吊销后，进程调用 EMQX 管理接口断开该客户端。上报按租户、设备、载荷摘要去重。服务连接使用持久会话：消息写入 MySQL 后才确认，未确认的消息可从 `mqtt_inbox` 恢复。入站队列上限 256 条。依赖短时不可用时，已收到的消息留在队头按顺序重试，服务连接保持重连。

## 观看、通话和本机 SFU

票据有效期 12 小时，库里保存摘要。换票后旧票据不能再授权新的观看或通话。已经加入的观看者和已经建立的通话留到本轮结束。

观看按设备、通道、流类型和清晰度复用会话。同一客户端的幂等键返回同一次观看。观看者要心跳；`JOINING` 和 `ACTIVE` 只在宽限期内占用。会话状态包括 `CREATED`、`WAITING_DEVICE`、`STREAM_STARTING`、`ACTIVE`、`STOP_PENDING`、`STOPPING`、`CLOSED`、`FAILED`。凭证签发本身不会把会话标成 `ACTIVE`。

同一设备同时只有一路主通话，占用从呼叫开始保持到终态。被叫必须是另一名具有 `call` 权限的用户。只有被叫可以接听或拒绝。主叫可以取消振铃，并可以结束已接听或已连接的通话。通话状态包括 `RINGING`、`ACCEPTED`、`CONNECTED`、`REJECTED`、`TIMEOUT`、`ENDED`、`FAILED`。

开流和关流读取 `rtc_control_plan` 里的测试结果。空结果按成功解释。这套计划不向固件发送视频指令。后台任务放在 MySQL，用 `FOR UPDATE SKIP LOCKED` 领取，并用 `lease_token` 核对完成和重试。默认延迟关流 30 秒，观看加入宽限 15 秒，振铃超时 30 秒。

未设置 `IOT_SFU_ADDR` 时，观看返回 `provider=test`、`connectable=false`。设置之后，开流任务会在本机 Pion SFU 上建立房间，观看接口签发最多两分钟的可连接凭证，并带上 `whep_url`。SFU 只转发 RTP，只走本机 IPv4，关闭 mDNS。`viewer:` 只能订阅，`device:` 发布公开流，`call:` 发布通话上行且不转给观看者。通话 HTTP 接口不返回发布地址。WHIP 和 WHEP 的 SDP 交换在 SFU 自己的监听上：`POST`、`GET`、`PUT` `/whip/{roomID}` 和 `/whep/{roomID}`。

## 启动

空配置：

```sh
export GOTOOLCHAIN=local
go run ./cmd/server
```

默认业务监听 `127.0.0.1:8080`。

本机全套：

```sh
export GOTOOLCHAIN=local
sh scripts/dev-up.sh
set -a
. ./.env
set +a
go run ./cmd/server
```

`scripts/dev-up.sh` 会在缺少 `.env` 时生成口令，再拉起只绑定 `127.0.0.1` 的 MySQL、Redis 和 EMQX。生成出的业务地址是 `127.0.0.1:18080`，钩子 `127.0.0.1:18082`，SFU `127.0.0.1:18090`。

指定构建版本：

```sh
VERSION=b02 REVISION=local sh scripts/build.sh
IOT_INSTANCE_ID=local-1 IOT_CONFIG_VERSION=dev ./bin/server
```

## 探针

业务监听上的探针不需要登录。

| 方法 | 路径 | 含义 |
|---|---|---|
| GET, HEAD | `/health/live` | 进程可响应时返回 200，正文 `status=live` |
| GET, HEAD | `/health/ready` | 正在服务时返回 200。退出过程中，或已配置的 MySQL、Redis、MQTT 有任一不可用时，返回 503。正文含 `mqtt_subscribed` 和 `command_attention` |
| GET, HEAD | `/version` | `instance_id`、`config_version`、`build_version`、`revision`、`go_version` |

业务接口挂上后，`GET /metrics` 返回进程内 Prometheus 文本。指标有认证拒绝、入站、命令超时、踢线结果、待人工处理的命令数、MySQL、Redis、MQTT 是否可用，以及构建信息。标签不含 `device_id`。60 秒内第 5 次认证拒绝会打一条 `alert=mqtt_auth_deny_spike`。

## 业务接口

除登录外都要 Bearer 令牌。失败正文是 `{"error":{"code":"...","message":"..."}}`，`message` 与 `code` 相同。

| 方法 | 路径 | 作用 |
|---|---|---|
| POST | `/api/v1/auth/login` | 登录，返回令牌和用户 |
| GET | `/api/v1/devices` | 当前用户可见的设备 |
| POST | `/api/v1/devices` | 平台管理员登记设备 |
| GET | `/api/v1/devices/{id}` | 设备 |
| GET | `/api/v1/devices/{id}/status` | `mqtt_online`、`rtc_connected`、`track_ready` 和设备自报状态 |
| GET | `/api/v1/devices/{id}/capabilities` | 功能点 |
| GET | `/api/v1/devices/{id}/properties` | 当前属性 |
| POST | `/api/v1/devices/{id}/commands` | 下发一路开关，202 |
| GET | `/api/v1/commands/{commandID}` | 命令状态 |
| POST | `/api/v1/commands/{commandID}/retry` | 重试 `uncertain` 或 `needs_attention` |
| POST | `/api/v1/commands/{commandID}/close` | 把上述状态收成 `failed` |
| POST | `/api/v1/devices/{id}/grants` | 增加 `manage`、`view`、`call` 或 `ticket` |
| DELETE | `/api/v1/devices/{id}/grants` | 收回一项授权 |
| POST | `/api/v1/devices/{id}/topic-model` | 把主题代次改成 `1_0pro` 或 `2_0_test` |
| POST | `/api/v1/devices/{id}/disable` | 禁用并尝试踢线 |
| POST | `/api/v1/devices/{id}/enable` | 重新启用 |
| POST | `/api/v1/devices/{id}/revoke` | 吊销接入资格并尝试踢线 |
| POST | `/api/v1/devices/{id}/tickets` | 签发票据 |
| POST | `/api/v1/devices/{id}/tickets/rotate` | 换票 |
| POST | `/api/v1/devices/{id}/watch` | 加入观看 |
| POST | `/api/v1/viewers/{viewerID}/heartbeat` | 观看心跳 |
| POST | `/api/v1/viewers/{viewerID}/leave` | 退出观看 |
| GET | `/api/v1/sessions/{sessionID}` | 会话 |
| POST | `/api/v1/devices/{id}/calls` | 发起主通话 |
| POST | `/api/v1/calls/{callID}/accept` | 被叫接听 |
| POST | `/api/v1/calls/{callID}/reject` | 被叫拒绝 |
| POST | `/api/v1/calls/{callID}/end` | 结束通话 |
| GET | `/api/v1/calls/{callID}` | 通话 |

`/internal/` 只出现在 `IOT_HOOK_ADDR`。请求要带 `X-Iot-Hook-Token`。业务监听和钩子地址相同则配置无效。

| 方法 | 路径 | 作用 |
|---|---|---|
| POST | `/internal/mqtt/auth` | EMQX 认证，返回 allow 或 deny |
| POST | `/internal/mqtt/acl` | EMQX 主题授权 |
| POST | `/internal/media/events` | 媒体事件。`track_published` 把媒体标为就绪并尝试进入 `ACTIVE`。`track_unpublished` 和 `room_closed` 会中断尚未结束的会话 |
| PUT | `/internal/rtc/plan` | 写入开流、关流或状态的测试结果。要 `IOT_ENABLE_TEST_CONTROLS=1` |

## 配置

环境变量覆盖 `IOT_CONFIG_FILE` 指向的 `KEY=VALUE` 文件。模板见 `configs/config.env.example`，那里只有空进程所需的键。启动失败时只输出失败的配置键。日志会屏蔽键名中含 `password`、`secret`、`token`、`credential` 的值，也会替换已知的长密钥。

| 键 | 默认 | 约束 |
|---|---|---|
| `IOT_HTTP_ADDR` | `127.0.0.1:8080` | 打开 MQTT 时主机必须是回环地址 |
| `IOT_HOOK_ADDR` | 不监听 | 回环或私网地址，且与业务监听不同 |
| `IOT_SFU_ADDR` | 不监听 | 回环地址，且与业务监听、钩子都不同 |
| `IOT_INSTANCE_ID` | 主机名和进程号 | 服务 MQTT 客户端 ID 由它生成 |
| `IOT_CONFIG_VERSION` | `dev` | 只出现在版本探针和启动日志 |
| `IOT_LOG_LEVEL` | `info` | `debug`、`info`、`warn`、`error` |
| `IOT_SHUTDOWN_TIMEOUT` | `10s` | 正数时长 |
| `IOT_TENANT_ID` | `dev` | 只能是 `dev` |
| `IOT_COMMAND_DEADLINE` | `5s` | 1 秒到 30 秒 |
| `IOT_STOP_DELAY` | `30s` | 1 秒到 2 分钟 |
| `IOT_VIEWER_JOIN_GRACE` | `15s` | 1 秒到 2 分钟 |
| `IOT_CALL_RING_TIMEOUT` | `30s` | 1 秒到 2 分钟 |
| `IOT_ENABLE_TEST_CONTROLS` | 关闭 | `1` 或 `true` 才挂测试计划入口 |
| `IOT_SEED_ADMIN_PASSWORD` | 不建用户 | 有值时创建平台管理员 `admin` |
| `IOT_SEED_OPERATOR_PASSWORD` | 不建用户 | 有值时创建只有查看权的 `operator` |

依赖按组出现：一组里有一个键，其余键也必须有值。MySQL 地址必须是回环，用户必须是 `iot_app`，库必须是 `iot_dev`。Redis 地址必须是回环。MQTT Broker 必须是回环上的 `tcp` 或 `mqtt`。服务用户名必须是 `iot-dev-service`，设备用户名必须是 `root`。EMQX 管理接口必须是回环上的 `http`，端口 `18083`。

| 组 | 键 |
|---|---|
| MySQL | `IOT_MYSQL_ADDR`、`IOT_MYSQL_USER`、`IOT_MYSQL_PASSWORD`、`IOT_MYSQL_DATABASE` |
| Redis | `IOT_REDIS_ADDR`、`IOT_REDIS_PASSWORD` |
| MQTT | `IOT_MQTT_BROKER_URL`、`IOT_MQTT_SERVICE_USERNAME`、`IOT_MQTT_SERVICE_PASSWORD`、`IOT_MQTT_DEVICE_USERNAME`、`IOT_MQTT_DEVICE_PASSWORD`、`IOT_MQTT_HOOK_TOKEN`、`IOT_EMQX_API_URL`、`IOT_EMQX_API_USER`、`IOT_EMQX_API_PASSWORD` |

首次迁移会写入产品 `controller`、样例设备 `94a990cb56a8` 和七个功能点。提供了种子口令时创建 `admin` 和 `operator`，并给这台样例设备授权。重启不改已有口令哈希。

## 本机依赖

容器名前缀是 `iot-dev`。镜像为 MySQL 8.4.13、Redis 8.10.3、EMQX 6.3.3。端口只绑定 `127.0.0.1`：MySQL 13306、Redis 16379、MQTT 11883、Dashboard 18083。内存上限 MySQL 1024MB、Redis 256MB、EMQX 512MB。口令写在 gitignore 的 `.env`，脚本不打印值。相关变量还有 `IOT_MYSQL_ROOT_PASSWORD`、`EMQX_DEV_DASHBOARD_PASSWORD`、`EMQX_DEV_APP_PASSWORD`、`EMQX_NODE_COOKIE`。

```sh
export GOTOOLCHAIN=local
sh scripts/dev-up.sh
sh scripts/check.sh
sh scripts/integration.sh
```

`scripts/check.sh` 不连接容器。`scripts/integration.sh` 读取 `.env`，用模拟客户端跑 MQTT 闭环和控制平面，并在结束时拉起被它停过的 MySQL 和 Redis。`sh scripts/dev-down.sh` 停止容器并保留数据卷。

## 检查

```sh
sh scripts/check.sh
```

该脚本依次执行 `gofmt -l`、`go test ./...`、`go vet ./...` 和 `go test -race ./...`。默认测试不含 `integration` 标签。GitHub Actions 跑同一组检查，并执行 `go build ./cmd/server`。

## 当前边界

这套进程能在本机完成设备接入、属性与开关命令、观看和通话记账，以及本机房间内的 RTP 转发。下面这些仍在进程之外：

- 每台设备独立的 MQTT 凭证和 TLS。现在所有设备共用 `root`。
- 独立的 APP MQTT 身份。`app:` 前缀会被拒绝。
- 固件侧的视频采集和开流协议。测试计划只决定控制平面怎么记账。
- 真实 APP、真实媒体服务、公网 Broker，以及生产拓扑。依赖地址被限制在本机，EMQX 管理端口固定为 18083。
- 真实设备和真实 APP 的联调验收。
