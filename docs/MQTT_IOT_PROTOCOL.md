# 宠智灵智能项圈 1.0pro — MQTT 数据接口（V1.1）

依据《IoT设备控制指令需求》V1.1（2026-10-14）。  
服务器由宏 `PETCOLLAR_MQTT_SERVER_PRIVATE` 选择（见 `petcollar_config.h`）。

`{mac}` = 设备 MAC（去冒号；私有服为**小写**，如 `9030d6cb176c`）。

## 1. Topic

### 私有服（`PETCOLLAR_MQTT_SERVER_PRIVATE=1`，当前默认）

| Topic | 方向 | 说明 |
|---|---|---|
| `pet/collar/1_0pro/{mac}/cmd` | 云 → 设备 | 控制指令 |
| `pet/collar/1_0pro/{mac}/state` | 设备 → 云 | 在线 / cmd_result / location / DP |

Host `116.169.215.214:11883`，明文 MQTT；账号见配置宏。

### EMQX Cloud（`PETCOLLAR_MQTT_SERVER_PRIVATE=0`）

| Topic | 方向 | 说明 |
|---|---|---|
| `petcollar_1pro/{mac}/cmd` | 云 → 设备 | 控制指令 |
| `petcollar_1pro/{mac}/telemetry` | 设备 → 云 | cmd_result / location / DP |
| `petcollar_1pro/{mac}/status` | 设备 → 云 | retained 在线 |

账号：`PETCOLLAR_DEVICE_ID` / `PETCOLLAR_DEVICE_SECRET`。

## 2. 下行指令（cmd）

直接控制，无 `msgId`：

```json
{"cmd":"trip_track","on":true,"params":{"interval_min":5}}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| cmd | string | 是 | 见第 3 节 |
| on | bool | 多数指令 | `true` 开 / `false` 关；一次性指令可省略 |
| params | object | 视指令 | 参数表 |
| ts | number | 否 | Unix 秒（可忽略） |

## 3. 各命令功能说明

### CMD-01 · `find_light`（寻宠闪灯）

**功能**：控制项圈 LED 闪烁，辅助用户在近处寻找宠物/项圈。  
**场景**：App 寻宠页开/关闪灯。  
**行为**：`on:true` 按颜色、频率闪烁，到 `duration_min` 自动结束或 `on:false` 立即停止；设备上报 `accepted` / `ended`。  
**params**：`color`(u32)、`freq_hz`(1=慢闪 / 3=快闪)、`duration_min`(0–60，默认 1)。

```json
{"cmd":"find_light","on":true,"params":{"color":16711680,"freq_hz":3,"duration_min":1}}
```

### CMD-02 · `buzzer`（寻宠响铃，扬声器 SPK 实现）

**功能**：用项圈扬声器播放提示音，辅助用声音定位项圈（无独立蜂鸣器硬件）。  
**场景**：App 触发寻宠响铃。  
**行为**：`on:true` 按音调与次数经 SPK 发声（约 120ms 响 / 280ms 间隔）；次数用尽或 `on:false` 停止；通话中返回 `busy`。  
**params**：`tone_hz`(2000=慢响 / 3000=快响)、`count`(1–60，默认 10)。

```json
{"cmd":"buzzer","on":true,"params":{"tone_hz":2000,"count":10}}
```

### CMD-03 · `gps_oneshot`（GPS 立即上报位置）

**功能**：立刻做一次 GPS 定位并上报，不进入持续周期策略。  
**场景**：出行开始/结束、进入实时定位时各触发一次。  
**行为**：设备取当前（或最新）定位点，经 `telemetry` 发 `type=location`（`reason=oneshot`），并回 `cmd_result`（`success` / `failed`）。无有效星历或天线开路时为 `failed`（如 `code=-30`）。  
**params**：无。

```json
{"cmd":"gps_oneshot","on":true}
```

### CMD-04 · `find_mode`（寻宠模式）

**功能**：宠物走失时开启高频定位，在设定时长内按短间隔持续上报位置。  
**场景**：用户主动开/关寻宠高频跟踪。  
**行为**：开启后按 `interval_s` 周期上报 `location`（`reason=find`）；到 `duration_min` 或 `on:false` 退出，并同步 `mode_state`。与出行同时开时，**寻宠优先**。  
**params**：`duration_min`(1–300，默认 5)、`interval_s`(2–30，默认 5)。

```json
{"cmd":"find_mode","on":true,"params":{"duration_min":5,"interval_s":5}}
```

### CMD-05 · `trip_track`（出行上报位置频率）

**功能**：出行期间按较长周期上报位置，供 App 绘制轨迹。  
**场景**：开始/结束出行。  
**行为**：`on:true` 启用周期上报（`reason=trip`），并**立即**附带一次 oneshot；`on:false` 再附带一次 oneshot 后停止周期策略。周期上报不能替代开/关时的即时定位。  
**params**：`interval_min`(1–60，默认 5)。

```json
{"cmd":"trip_track","on":true,"params":{"interval_min":5}}
```

### CMD-06 · `activate`（终端设备激活）

**功能**：设备与 App 配对/激活流程中，确认并启动 GPS、IMU（及能力范围内的相关外设）。  
**场景**：首次绑定或重新激活。  
**行为**：执行激活后回 `cmd_result`（`success` / `failed`），业务侧据此判断是否可进入正常使用。  
**params**：无。

```json
{"cmd":"activate","on":true}
```

### CMD-07 · `mic_record`（麦克风录音）

**功能**：按指定时长开启麦克风录音，用于宠物声音行为分析等业务。  
**场景**：用户触发“听一听 / 声音翻译”类功能。  
**行为**：到 `duration_s` 或 `on:false` 结束录音并上报结果；**录音完成 ≠ AI 分析完成**（音频如何上传分析需另行约定）。与扬声器/通话等互斥，忙则 `busy`。  
**params**：`duration_s`(1–60，默认 10)。

```json
{"cmd":"mic_record","on":true,"params":{"duration_s":10}}
```

### CMD-08 · `speaker_play`（扬声器播放）

**功能**：通过项圈扬声器播放设备内置预设音频（安抚音、提示音等）。  
**场景**：业务下发播放某条预设音。  
**行为**：播到文件结束自然停止，或 `on:false` 打断；时长跟随文件，不单独配置。上报开始/结束/失败。  
**params**：`file`（可选预设名，缺省用设备默认预设）。

```json
{"cmd":"speaker_play","on":true,"params":{"file":"preset.wav"}}
```

### CMD-09 · `voice_call`（项圈语音通话）

**功能**：App 与项圈经蜂窝网络做实时双向语音（声网 RTC）；设备打开麦克与扬声器并加入频道。  
**场景**：远程对宠说话 / 听环境声。  
**行为**：`on:true` 携带 `channel`/`uid`/`token` 发起加入；须区分「指令已受理 `accepted`」与「声网实际接通」（见通话状态）；超时、`on:false` 或失败则挂断并解除音频占用。  
**params**：`channel`、`uid`、`token`、`duration_min`(1–60，默认 15)。

```json
{"cmd":"voice_call","on":true,"params":{"channel":"936193","uid":1001,"token":"...","duration_min":15}}
```

兼容旧格式（等价本命令）：

```json
{"data":{"agora_call":true,"channel":"936193","uid":1001,"token":"..."}}
```

### 命令一览

| 编号 | cmd | 功能一句话 |
|---|---|---|
| CMD-01 | `find_light` | LED 闪灯辅助寻宠 |
| CMD-02 | `buzzer` | 扬声器响铃寻宠 |
| CMD-03 | `gps_oneshot` | 立刻上报一次 GPS |
| CMD-04 | `find_mode` | 走失高频持续定位 |
| CMD-05 | `trip_track` | 出行周期轨迹定位 |
| CMD-06 | `activate` | 激活并启动 GPS/IMU |
| CMD-07 | `mic_record` | 按时长录音 |
| CMD-08 | `speaker_play` | 播放内置预设音 |
| CMD-09 | `voice_call` | 声网双向语音通话 |

## 4. 上行 telemetry（按 type）

### 4.1 指令结果 `cmd_result`

```json
{"ts":1760580001,"type":"cmd_result","cmd":"trip_track","result":"accepted","code":0}
```

| result | 含义 |
|---|---|
| `accepted` | 已受理并开始（持续任务） |
| `success` | 一次性操作成功 |
| `failed` | 执行失败 |
| `ended` | 持续任务正常结束 |
| `busy` | 音频资源占用，拒绝 |

### 4.2 定位 `location`

```json
{"ts":1760580002,"type":"location","reason":"oneshot","lat":22.54,"lon":113.94,"sats":8,"valid":true}
```

| reason | 来源 |
|---|---|
| `oneshot` | CMD-03，或出行开/关附带的即时定位 |
| `find` | CMD-04 寻宠周期 |
| `trip` | CMD-05 出行周期 |

`valid=false`：本次无有效定位（与历史轨迹点区分）。

### 4.3 模式状态 `mode_state`

```json
{"type":"mode_state","find_mode":true,"trip_track":false,"find_remain_s":280,"trip_interval_s":300}
```

用于同步寻宠/出行是否真正开启及剩余时间等。

### 4.4 其它 DP（兼容）

运动等：`{"data":{"pet_action":"static"}}`

## 5. 跨指令规则

1. **出行 + 即时定位**：`trip_track` 开/关各触发一次 oneshot；周期点不能替代这两次。
2. **寻宠优先于出行**：同时开启时按寻宠间隔上报；寻宠结束后若出行仍开则恢复出行间隔。
3. **音频互斥**（CMD-02/07/08/09）：`voice_call` 可抢占；其它忙则回 `busy`，不排队缓存。
4. **离线**：设备侧不缓存指令；平台超时未收到 `cmd_result` 按失败/离线处理。
5. **持续任务结束**：用户关闭、超时、异常均需上报，App 不得继续显示“进行中”。

## 6. 常用示例

```json
{"cmd":"gps_oneshot","on":true}
```

```json
{"cmd":"trip_track","on":true,"params":{"interval_min":5}}
```

```json
{"cmd":"find_mode","on":true,"params":{"duration_min":5,"interval_s":5}}
```

```json
{"cmd":"voice_call","on":true,"params":{"channel":"936193","uid":1001,"token":"..."}}
```

```json
{"cmd":"trip_track","on":false}
```
