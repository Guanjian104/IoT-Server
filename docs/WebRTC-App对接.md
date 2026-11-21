# App WebRTC 通话对接

App 和一台项圈在同一间 SFU 房间里互发音频。媒体是 UDP 上的 RTP。App 不连接 EMQX。

下面的地址是 cs 联调使用的一组值。

| 用途 | 地址 |
|---|---|
| App 访问的 WHIP | `http://120.25.100.77:18196` |
| ICE | `120.25.100.77` 上的 UDP `40100–40131` |

`127.0.0.1:18096` 是进程自己的监听，不发给 App。观看用的 WHEP 和涂鸦的 `/devices/:id/webrtc-ticket` 不用于这路通话。

## 1. 拿到本次凭证

下发开启命令：

`POST /api/v1/devices/{mac}/commands`

```json
{"cmd":"voice_call","on":true,"params":{"duration_min":15}}
```

`{mac}` 是 12 位小写 MAC。`duration_min` 可省略；出现时是 1–60，只写给设备，不写进 App 的凭证。

成功时 HTTP 202：

```json
{
  "command_id": "cmd-480ea22e26f0084c",
  "device_id": "9030d6cb2703",
  "state": "pending",
  "webrtc": {
    "whip_url": "http://120.25.100.77:18196/whip/pion-9030d6cb2703",
    "token": "<App 凭证>",
    "expires_at": "2026-11-20T10:07:36Z"
  }
}
```

| 字段 | 含义 |
|---|---|
| `webrtc.whip_url` | 本次房间的 WHIP 地址，和设备收到的是同一个 |
| `webrtc.token` | 只属于身份 `call:u<用户号>` 的发布凭证 |
| `webrtc.expires_at` | 凭证失效时间，UTC，从签发起 10 分钟 |

设备的凭证在 MQTT 的 `cmd` 里，身份是 `call:{mac}`。两张凭证不同。App 只用 `webrtc` 里的这一张，不订阅 `cmd`，也不使用 Device-Server 的登录令牌去访问 WHIP。

`state` 后来变成 `accepted`，只表示设备受理了命令，不表示音频已经接通。202 一返回就可以开始第 2 节。没有公网 SFU 时，这个接口返回 503，正文没有 `webrtc`。

挂断另发一条，没有 `params`：

```json
{"cmd":"voice_call","on":false}
```

发出后关闭本端 PeerConnection。

## 2. 用 WHIP 加入

建立 PeerConnection，添加一路要发送的音频，生成本端 offer。ICE 只收集 IPv4，关闭 mDNS。SDP 正文不超过 64 KiB。本端 HTTP 读超时要大于 10 秒。

`POST {whip_url}`

```http
Authorization: Bearer <App 凭证>
Content-Type: application/sdp

<offer SDP>
```

成功时返回 201，正文是 `application/sdp` 的 answer。把它设为远端描述。answer 里的主机候选地址是 `120.25.100.77`，端口落在 `40100–40131`。App 向这个地址发送 UDP。

| HTTP | 含义 |
|---|---|
| 201 | 已加入，正文是 answer。这个身份已在房间里时，上一轮连接被替换 |
| 401 | 凭证过期、房间不对，或角色不是发布 |
| 404 | 房间不存在 |
| 409 | 同一个身份的两次加入叠在一起 |
| 400 | SDP 无法接受 |

同一身份再次 POST 会替换上一轮。挂断命令不会清掉房间里的连接。

要听设备时，再向同一个 `whip_url` 发 `GET`，请求头仍是 `Authorization: Bearer <App 凭证>`。设备的音频进入房间后，响应 200，正文是新的 offer，最多等待 10 秒。没有新 offer 时返回 204，用同一张凭证继续 GET，直到 `expires_at`。

App 把这个 offer 设为远端描述，生成本端 answer，再 `PUT` 同一个地址：

```http
Authorization: Bearer <App 凭证>
Content-Type: application/sdp

<answer SDP>
```

成功返回 204。第一轮 answer 不会新增媒体行，所以听见设备必须完成 GET 和 PUT。设备也要完成它自己的这一轮，App 才能被听见。

凭证只用于这一间房间的发布。不要拿它去请求 `/whep/`。这路 `call:` 音频不转给观看会话。
