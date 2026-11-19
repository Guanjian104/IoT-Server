这是上一版中控的报文，当前进程不再使用这些主题。项圈主题和字段见 `MQTT_IOT_PROTOCOL.md`。

当前这台中控的 `deviceId` 是 `94a990cb56a8`。换一块板子会变成那块板子的 Wi-Fi STA MAC，12 位小写十六进制，不带冒号。客户端 ID 与 `deviceId` 相同。

## 连接

| 项 | 值 |
|---|---|
| 地址 | `mqtt://116.169.215.214:18087` |
| 协议 | MQTT 3.1.1，明文 TCP，不校验证书 |
| 用户名 | `root` |
| 密码 | 与固件 `SELF_MQTT_PASS` 相同 |
| 客户端 ID | `deviceId`，例如 `94a990cb56a8` |
| 保活 | 60 秒 |

端口必须先接受 TCP。连不上时串口是 `Error transport connect`，此时还没有发出用户名和密码。

## 主题

把 `{deviceId}` 换成该中控的 MAC。下面用当前这块板举例。QoS 都是 1。

| 方向 | 主题 | 保留 |
|---|---|---|
| 中控上报 | `device/94a990cb56a8/property/report` | 否 |
| 后端下发 | `device/94a990cb56a8/property/set` | 否 |
| 中控应答 | `device/94a990cb56a8/property/set_reply` | 否 |
| 在线状态 | `device/94a990cb56a8/status` | 是 |

`status` 连上后发布 `online`。异常断开时遗嘱是 `offline`。后端用保留消息判断这台中控是否在线。

## 功能点

插座在屏幕上绑定的中文名必须和下表完全一致。没绑到该名字的开关不会出现在上报里。同一名字只能有一只插座，最多 6 只。

| 名称 | 标识符 | 类型 | 方向 |
|---|---|---|---|
| 温湿度 | `temp_humidity_sens_2` | 字符串 | 仅上报 |
| 照明 | `switch_light` | 布尔 | 上报 + 下发 |
| 雾化 | `switch_mist` | 布尔 | 上报 + 下发 |
| 通风 | `switch_fan` | 布尔 | 上报 + 下发 |
| 喂食 | `switch_feed` | 布尔 | 上报 + 下发 |
| 加热垫 | `switch_heat` | 布尔 | 上报 + 下发 |
| 晒背灯 | `switch_bask` | 布尔 | 上报 + 下发 |

给温湿度下发指令会被忽略，不改变传感器。

## 温湿度字符串

一条字符串同时带两个区，分号隔开。`H` 是热区，`C` 是冷区。

```text
H:23.7C,49%;C:23.8C,48%
```

- 温度一位小数，单位写在数字后面，接着逗号和湿度整数、百分号。
- 该区没有 2 分钟内的采样时，对应半边是 `--`。例如只有冷区：`H:--;C:23.1C,49%`。
- 两个区都没有新采样时，这一项整段不报。

采样大约每分钟一条。数值没变时，20 秒内的重复上报会被丢掉。

## 设备上报

连上后先报一次当前快照，之后随采样和开关变化再报。多只插座在几百毫秒内一起变化时，会收成一条。

```json
{
  "deviceId": "94a990cb56a8",
  "msgId": "s3-12",
  "time": 1762070400000,
  "data": {
    "temp_humidity_sens_2": {
      "value": "H:23.7C,49%;C:23.8C,48%",
      "time": 1762070400000
    },
    "switch_light": { "value": true, "time": 1762070400000 },
    "switch_mist": { "value": false, "time": 1762070400000 },
    "switch_fan": { "value": true, "time": 1762070400000 }
  }
}
```

- `time` 为毫秒。中控已对时用墙上时间，未对时用开机后的毫秒，后端不要把后者当成日期。
- `msgId` 形如 `s3-` 加递增序号。
- 布尔值是 JSON 的 `true` / `false`，没有引号。
- 未绑定的开关不出现。不要把「字段缺失」当成「关闭」。

## 后端下发

向 `device/{deviceId}/property/set` 发布。下面几种写法都可以，布尔也可以写成 `{"value":false}`、`0` / `1` 或字符串 `"true"` / `"1"`。

```json
{
  "msgId": "srv-1",
  "time": 1762070400000,
  "data": {
    "switch_light": false
  }
}
```

也可以把开关直接放在根上，或放进 `properties` 对象。`properties` 如果是字符串，里面必须仍是 JSON，例如 `"{\"switch_light\":false}"`。

中控只处理上表里的六个开关。报文里没有任何一个开关字段时，不执行，也不应答。

## 执行结果

成功或失败都发到 `device/{deviceId}/property/set_reply`：

```json
{
  "deviceId": "94a990cb56a8",
  "msgId": "srv-1",
  "time": 1762070401000,
  "code": 0
}
```

| code | 含义 |
|---|---|
| 0 | 已发给对应插座 |
| 1001 | 值无法识别，或插座当前不在线、控制失败 |
| 1002 | 这个名字还没有绑定插座 |

`code` 为 0 时，中控会再发一条 `property/report`，用同一份快照确认新状态。
