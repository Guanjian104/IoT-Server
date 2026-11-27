# 设备上行 RTP 诊断

这项功能用来看项圈发给服务器的语音包，以及服务器转给 App 的是不是同一份内容。默认关闭。只看设备到 App 这一路，不改通话编码，也不在转发过程里转码。

## 包是在哪里看到的

项圈的语音在 `pump` 里从 `TrackRemote.Read` 读出来。这时 DTLS/SRTP 已经解开，读到的是 RTP，不是加密包。

紧接着 `TrackLocalStaticRTP.Write` 把同一份字节交给已经订了这条轨的 App。Pion 给每个 App 重写 SSRC 和 Payload Type，不改 Payload，序号和时间戳也留着。

诊断挂在这两步旁边：

- 入站：`Read` 之后的 RTP。来源是 `call:` 开头、但不是 `call:u` 的设备，而且是音频。
- 出站：每个 App 订阅者各记一条。App 的身份是 `call:u` 开头。一个设备转给多个 App 时，每条出站都带同一个 `packet_id`。
- 没有 App，或者这条发送轨还没绑上，记 `MISSING_OUTBOUND`。
- 头变了但 Payload 的 SHA256 没变，结论仍是 `IDENTICAL`，另外记下头被改写。

调试关掉时，这里只判断指针是空的，然后按原来的方式转发。

## 打开和关闭

接口在钩子监听上，不在公网 WHIP 端口。地址用进程的 `IOT_HOOK_ADDR`。请求头 `X-Iot-Hook-Token` 用现有的钩子令牌。

会话 ID 用项圈 MAC，例如 `9030d6cb2703`。房间 ID `pion-9030d6cb2703` 也可以。通话要先把房间建起来。

只记日志，不落 Payload：

```bash
curl -sS -X POST "http://$IOT_HOOK_ADDR/internal/sessions/9030d6cb2703/rtp-debug" \
  -H "Content-Type: application/json" \
  -H "X-Iot-Hook-Token: $IOT_MQTT_HOOK_TOKEN" \
  -d '{"log":true,"seconds":8,"max_packets":500}'
```

同时把 Payload 存下来。`capture` 不写就是关的，必须显式打开：

```bash
curl -sS -X POST "http://$IOT_HOOK_ADDR/internal/sessions/9030d6cb2703/rtp-debug" \
  -H "Content-Type: application/json" \
  -H "X-Iot-Hook-Token: $IOT_MQTT_HOOK_TOKEN" \
  -d '{"log":true,"capture":true,"seconds":8,"max_packets":500,"max_bytes":1048576}'
```

看报告：

```bash
curl -sS "http://$IOT_HOOK_ADDR/internal/sessions/9030d6cb2703/rtp-debug" \
  -H "X-Iot-Hook-Token: $IOT_MQTT_HOOK_TOKEN"
```

提前停下：

```bash
curl -sS -X DELETE "http://$IOT_HOOK_ADDR/internal/sessions/9030d6cb2703/rtp-debug" \
  -H "X-Iot-Hook-Token: $IOT_MQTT_HOOK_TOKEN"
```

`seconds` 不写是 8 秒，最长 30 秒。`max_packets` 不写是 500。`max_bytes` 不写是 1 MiB。到点、到包数、到文件大小、手动停止或房间关闭，都会自己停。

同一会话正在记录时再开一次会返回冲突。

日志行是 `RTP_UPLINK` 和 `RTP_FORWARD`。Payload 只留前 16 字节的十六进制和 SHA256，不写整段音频。

## 文件

默认目录是容器里的 `/tmp/rtp-debug`。可以用 `IOT_RTP_DEBUG_DIR` 换成别的绝对路径。

```text
/tmp/rtp-debug/{会话}/
  report.txt
  inbound/metadata.jsonl
  inbound/payload.bin
```

`payload.bin` 只有在 `capture` 为 true 时才有。里面是按到达顺序拼起来的 Payload，没有 RTP 头，也没有 SRTP。`metadata.jsonl` 里的 `payload_offset` 和 `payload_length` 能把每一包切出来。按序号恢复顺序时要先按到达顺序把 16 位序号展开，再排序；序号从 65535 回到 0 不是丢包。

从 cs 的容器拷出来：

```bash
docker cp eco-device-server:/tmp/rtp-debug/9030d6cb2703 ./data/rtp-debug/9030d6cb2703
```

`data/rtp-debug/` 不进入 git。这些文件只为这一次诊断，用完可以删。

## 报告在回答什么

`report.txt` 和 GET 的 `text` 里有这几句：

- PT 是不是 9。
- 时间戳增量是不是经常为 160。G.722 在 SDP 里写 `G722/8000`，8000 是 RTP 时钟，20ms 一帧就是 160。它不是 PCM 的 16000Hz。
- Payload 长度的最小、最大和平均。640 字节约是编码前 20ms 的 PCM，不是 G.722 包该有的长度。
- 丢包、乱序、重复。
- 入站和出站 Payload 的一致率，以及头被改写但 Payload 没变的次数。
- SFU 转给 App 的内容是否完整。这只说明服务器发出去的 Payload 和收到的一致，不代表手机网卡收到了每一个包。
- 是不是 G.722：不能确认。PT=9 和增量 160 只说明封包像 G.722/8000 的 20ms 音频。这次没有把 Payload 解码成 WAV。

队列满了会丢掉调试记录，转发继续。丢掉的数量在报告里。
