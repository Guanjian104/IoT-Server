# 项圈 WebRTC 对接

项圈用 MQTT 接收加入信息，再用 HTTP 向本进程的 SFU 交换 SDP。媒体是 UDP 上的 RTP。业务 HTTP、登录令牌、MySQL 和 Redis 不发给设备。

下面的地址是 cs 联调使用的一组值。进程监听仍是回环，公网入口由宿主机转到这个监听。

| 用途 | 地址 |
|---|---|
| 进程里的 SFU | `127.0.0.1:18096` |
| 设备访问的 WHIP | `http://120.25.100.77:18196` |
| ICE | `120.25.100.77` 上的 UDP `40100–40131` |

宿主机把 `120.25.100.77:18196` 的 TCP 反代到 `127.0.0.1:18096`。反代读超时大于 10 秒。安全组放行 TCP `18196` 和 UDP `40100–40131`。不要放行 `40000–40063`，那段已经由 eco-iot 使用。UDP 不能走这条 HTTP 反代。

对应配置：

```text
IOT_SFU_ADDR=127.0.0.1:18096
IOT_SFU_PUBLIC_IP=120.25.100.77
IOT_SFU_PUBLIC_URL=http://120.25.100.77:18196
IOT_SFU_UDP_MIN=40100
IOT_SFU_UDP_MAX=40131
```

## 1. 收到开启命令

主题：`pet/collar/1_0pro/{mac}/cmd`。`{mac}` 是 12 位小写 MAC。

```json
{"cmd":"voice_call","on":true,"params":{"whip_url":"http://120.25.100.77:18196/whip/pion-9030d6cb176c","token":"<凭证>","duration_min":15}}
```

| 字段 | 含义 |
|---|---|
| `whip_url` | 这台设备本次通话的 WHIP 地址 |
| `token` | 只属于身份 `call:{mac}` 的发布凭证，10 分钟内有效 |
| `duration_min` | 可选。出现时是 1–60 的通话上限 |

受理后先在 `pet/collar/1_0pro/{mac}/state` 上报 `cmd_result`，`result` 为 `accepted`。`accepted` 只表示命令已受理，不表示媒体已经接通。

挂断命令没有 `params`：

```json
{"cmd":"voice_call","on":false}
```

收到后停止发送和接收，关闭 PeerConnection，再上报 `ended`。失败上报 `failed`，麦克风被占用上报 `busy`。

## 2. 用 WHIP 加入

设备建立 PeerConnection，添加一路要发送的音频，生成本端 offer。ICE 只收集 IPv4 host 候选，关闭 mDNS。SDP 正文不超过 64 KiB。

`POST {whip_url}`

```http
Authorization: Bearer <token>
Content-Type: application/sdp

<offer SDP>
```

成功时返回 201，正文是 `application/sdp` 的 answer。把它设为远端描述。answer 里的主机候选地址是 `120.25.100.77`，端口落在 `40100–40131`。同一身份再次 POST 会关掉上一轮连接并返回新的 answer。凭证过期、房间不对或角色不是发布，返回 401。两次加入叠在一起时返回 409。

要听对端时，再向同一个 `whip_url` 发 `GET`，请求头仍是 `Authorization: Bearer <token>`。对方的音频进入房间后，响应 200，正文是新的 offer，最多等待 10 秒。没有新 offer 时返回 204。设备用这个 offer 生成 answer，再 `PUT` 同一个地址，正文是 `Content-Type: application/sdp` 的 answer。成功返回 204。第一轮 answer 不会新增媒体行，所以听对方必须完成这一轮。

凭证只用于这一间房间的发布。不要把它交给观看端，也不要拿它去请求 WHEP。

## 3. 联调对端

发起命令的调用方在 HTTP 202 里收到自己的凭证，身份是 `call:u<用户号>`，`whip_url` 与设备相同。设备报文里没有这张凭证。对端按第 2 节的同一顺序加入后，双方可以互相听到。观看会话收不到这路通话。
