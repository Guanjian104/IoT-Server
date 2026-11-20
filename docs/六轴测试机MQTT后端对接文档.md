# petcollar2.0-test MQTT 后端对接文档

> 工程：ESP32-C3 `petcollar2.0-test`（ESP-IDF 5.3.4）  
> 遥测格式：与 PetCollar2.0 **TELEM_V1** 对齐（`main/telem_v1.*`）  
> 协议：MQTT **3.1.1**，明文 TCP  
> 配置源码：`main/app_config.h`、`main/mqtt_report.c`

---

## 1. 对接概览

| 角色 | 行为 |
|------|------|
| 设备 | STA 连 WiFi → 连 Broker；**PUB** 遥测；**SUB** 命令 |
| 后端 | 订阅 `.../state`（可通配）；解析 Base64(TELEM_V1)；需要时向 `.../cmd` 下发 JSON |

```
设备 ──PUB──► pet/collar/2_0_test/{mac12}/state ──► Broker ──► 后端
后端 ──PUB──► pet/collar/2_0_test/{mac12}/cmd   ──► Broker ──► 设备
```

- 仅 MQTT **已连通**时才上报；未连通只缓存，不清发失败包。  
- 同一 `client_id` 同时只允许一个会话（broker 会踢旧连接）。

---

## 2. 连接参数

| 参数 | 当前联调值 | 说明 |
|------|------------|------|
| Host | `120.25.100.77` | 见 `MQTT_BROKER_URI` |
| Port | `11884` | TCP 明文 |
| Username | `root` | `MQTT_USERNAME` |
| Password | （部署配置） | `MQTT_PASSWORD`，勿写入公开仓库副本 |
| ClientId | **STA MAC 12 位小写无冒号** | 例 `447b304e6530` |
| KeepAlive | `600` s | `MQTT_KEEPALIVE_S` |
| Clean Session | 是 | |
| PUB QoS | **0** | 遥测 |
| SUB QoS | **0** | 命令 |

URI 形态：`mqtt://120.25.100.77:11884`

---

## 3. 设备标识

| 字段 | 规则 | 示例 |
|------|------|------|
| WiFi STA MAC | 芯片烧录 | `44:7b:30:4e:65:30` |
| `{mac12}` | 小写、无冒号，**Topic / ClientId** | `447b304e6530` |

**后端主键**：从 Topic 取 `{mac12}`（与 ClientId 相同）。

---

## 4. Topic

### 4.1 设备 → 云（遥测）

```
pet/collar/2_0_test/{mac12}/state
```

后端订阅建议：

```
pet/collar/2_0_test/+/state
```

或单设备：

```
pet/collar/2_0_test/447b304e6530/state
```

### 4.2 云 → 设备（命令）

```
pet/collar/2_0_test/{mac12}/cmd
```

- Payload：UTF-8 JSON，长度 ≤ 256  
- 设备连接成功后自动 SUB

---

## 5. 上报策略（本工程）

| 项 | 行为 |
|----|------|
| 上报周期 `interval` | 默认 **30 s**（NVS 可改，范围 **0~30**；`0`=关闭上报） |
| 本地采样 | 活动 **10 Hz**；静止判活动仍 **5 Hz** |
| 入上报缓存 | 活动：每帧；静止：**1 Hz** 抽稀 |
| 包内容 | 本周期内缓存的全部六轴；超单包上限则**分包** |
| 单包上限 | `count ≤ 255`，且二进制 ≤ `TELEM_V1_BIN_MAX`(4096) |
| 缓存上限 | 320 帧；MQTT 未通时满则丢最旧 |

`period_ms` 字段：活动帧常用 `100`；静止抽稀常用 `1000`。  
同周期内若有活动再静止，以最后入缓存时写入的 `period_ms` 为准。

粗算包长（TYPE1）：

| 场景 | 帧数 N | 二进制 | Base64 约 |
|------|--------|--------|-----------|
| 静止 30 s × 1 Hz | 30 | 34+360=394 B | ~528 |
| 活动 30 s × 10 Hz | 300 | 需分包（255+45） | 较大 |
| 单帧 | 1 | 46 B | 64 |

---

## 6. 遥测载荷：TELEM_V1 + Base64

```text
MQTT payload = Base64( TELEM_V1_binary )   // 纯文本，无 JSON 外壳
```

当前固件主路径均为 **TYPE1 IMU_BATCH**。

### 6.1 Header（Byte0）

```text
bit7-6  Version   00 = V1
bit5-4  Msg Type  01 = IMU_BATCH（本工程）
bit3-0  Status    固定 0（不上报姿态枚举）
```

### 6.2 TYPE1 布局

| Offset | Len | 字段 |
|--------|-----|------|
| 0 | 1 | Header |
| 1 | 1 | bit7=Charging；bit6-0=Battery(0..100) |
| 2 | 1 | bit7=gps_valid；bit4-1=gps_quality |
| 3..4 | 2 | Temp `int16 LE` ×100℃；`0x8000`=无效 |
| 5..7 | 3 | Steps `uint24 LE` |
| 8..11 | 4 | Unix time `uint32 LE`（UTC 秒；SNTP 未同步时可能异常） |
| 12..27 | 16 | GPS 位打包（见下） |
| 28..29 | 2 | `sample_count` `uint16 LE` |
| 30..31 | 2 | `period_ms` `uint16 LE` |
| 32..33 | 2 | `pkt_seq` `uint16 LE`（从 1 递增，跨包连续） |
| 34.. | N×12 | 每帧 6×`int16 LE`：ax,ay,az,gx,gy,gz |

本工程联调阶段常见填充：battery/charging/gps/steps≈0；temp=`0x8000`；speed=`0xFFFF`；course=`0x1FF`；hdop=`0xFF`；gps_age=`255`。

### 6.3 GPS 16B（LSB first，本机通常全 0/无效）

| 字段 | bits | 说明 |
|------|------|------|
| lat_e6 | 28 | 有符号，`round(lat×1e6)` |
| lon_e6 | 29 | 有符号 |
| alt_cm | 24 | 有符号 cm |
| sats | 6 | |
| gps_age_s | 8 | 255=未知/≥255s |
| speed_cm_s | 16 | `0xFFFF`=未知 |
| course_deg | 9 | `0x1FF`=未知 |
| hdop_x10 | 8 | `0xFF`=未知 |

### 6.4 六轴物理量（QMI8658A）

固件量程：**±4 g** / **±512 dps**。

| 量 | 换算 |
|----|------|
| 加速度 (g) | `raw / 8192` |
| 角速度 (dps) | `raw / 64` |

---

## 7. 下行命令

### 7.1 修改上报周期

Topic：`pet/collar/2_0_test/{mac12}/cmd`

```json
{"cmd":"interval","sec":5}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `cmd` | string | 固定 `"interval"` |
| `sec` | number | `0`~`30`；`0`=关闭上报；写入 NVS |

未知 `cmd` 忽略；JSON 非法忽略。

---

## 8. 后端处理建议

1. 订阅 `pet/collar/2_0_test/+/state`。  
2. 从 Topic 解析 `{mac12}`。  
3. `Base64` 解码 → 校验 Header version/type → 解析 TYPE1。  
4. 用 `(mac12, unix_time, pkt_seq)` 去重/排序；同设备多包按 `pkt_seq` 拼 IMU。  
5. 按 `period_ms` 还原时间轴：第 i 帧相对包内起点约 `i * period_ms` ms（静止抽稀时为 1 Hz）。  
6. 下发命令务必打到**该设备**的 `/cmd`，MAC 必须与 `/state` 一致。

---

## 9. Python 解析示例

```python
import base64
import struct

def get_bits(buf, bitpos, nbits):
    v = 0
    for i in range(nbits):
        b = bitpos + i
        if buf[b >> 3] & (1 << (b & 7)):
            v |= (1 << i)
    return v, bitpos + nbits

def sign_extend(u, nbits):
    sign = 1 << (nbits - 1)
    return u - (1 << nbits) if (u & sign) else u

def unpack_gps(b16):
    p = 0
    lat, p = get_bits(b16, p, 28); lat = sign_extend(lat, 28)
    lon, p = get_bits(b16, p, 29); lon = sign_extend(lon, 29)
    alt, p = get_bits(b16, p, 24); alt = sign_extend(alt, 24)
    sats, p = get_bits(b16, p, 6)
    age, p = get_bits(b16, p, 8)
    spd, p = get_bits(b16, p, 16)
    crs, p = get_bits(b16, p, 9)
    hdop, p = get_bits(b16, p, 8)
    return {
        "lat": lat / 1e6, "lon": lon / 1e6, "alt_cm": alt,
        "sats": sats, "gps_age_s": age,
        "speed_cm_s": None if spd == 0xFFFF else spd,
        "course_deg": None if crs == 0x1FF else crs,
        "hdop_x10": None if hdop == 0xFF else hdop,
    }

def decode_telem_v1(b64: str):
    raw = base64.b64decode(b64)
    hdr = raw[0]
    ver, typ = (hdr >> 6) & 3, (hdr >> 4) & 3
    assert ver == 0 and typ == 1, (ver, typ)
    bat = raw[1] & 0x7F
    charging = (raw[1] >> 7) & 1
    fl = raw[2]
    gps_valid = (fl >> 7) & 1
    quality = (fl >> 1) & 0xF
    temp = struct.unpack_from("<h", raw, 3)[0]
    steps = raw[5] | (raw[6] << 8) | (raw[7] << 16)
    unix_t = struct.unpack_from("<I", raw, 8)[0]
    n, period, pkt_seq = struct.unpack_from("<HHH", raw, 28)
    frames = []
    off = 34
    for _ in range(n):
        ax, ay, az, gx, gy, gz = struct.unpack_from("<hhhhhh", raw, off)
        frames.append({
            "ax_g": ax / 8192.0, "ay_g": ay / 8192.0, "az_g": az / 8192.0,
            "gx_dps": gx / 64.0, "gy_dps": gy / 64.0, "gz_dps": gz / 64.0,
            "raw": [ax, ay, az, gx, gy, gz],
        })
        off += 12
    return {
        "Battery": bat, "Charging": charging,
        "gps_valid": gps_valid, "gps_quality": quality,
        "Temp": None if temp == -32768 else temp / 100.0,
        "Steps": steps, "unix_time": unix_t,
        "period_ms": period, "pkt_seq": pkt_seq,
        "imu": frames, **unpack_gps(raw[12:28]),
    }

# 下发示例
# client.publish("pet/collar/2_0_test/447b304e6530/cmd",
#                '{"cmd":"interval","sec":5}', qos=0)
```

---

## 10. MQTTX 联调检查清单

1. 连接 Broker：`120.25.100.77:11884`，用户 `root`，密码与固件一致。  
2. 订阅 `pet/collar/2_0_test/+/state`。  
3. 确认设备串口：`MQTT connected client=<mac12>`，且只出现**一次**。  
4. 收到 `/state` 后，对**同一 mac12** 发 `/cmd`：`{"cmd":"interval","sec":5}`。  
5. 串口应有 `cmd interval -> 5 s`；此后约每 5 s 一包，静止包 `count≈5`。

---

## 11. 与 PetCollar2.0 差异（对接注意）

| 项 | PetCollar2.0（Fish） | 本工程 2_0_test |
|----|----------------------|-----------------|
| Topic 前缀 | `device/telemetry/petcollar/v2/{mac}` | `pet/collar/2_0_test/{mac}/state` |
| 命令 Topic | `device/cmd/petcollar/{mac}` | `pet/collar/2_0_test/{mac}/cmd` |
| ClientId | 常为 `PetCollar_xxxx` | **完整 mac12** |
| 载荷 | TELEM_V1 Base64 | **相同二进制** |
| 上报策略 | 运动段 + 结段 hold | **固定 interval 缓存**；静止 1 Hz 入包 |

二进制解析可复用 PetCollar2.0 `TELEM_V1` 解码器；**Topic / ClientId 不可混用**。

---

## 12. 修订记录

| 日期 | 说明 |
|------|------|
| 2026-11-20 | 初版：对接 petcollar2.0-test 现行固件 |
