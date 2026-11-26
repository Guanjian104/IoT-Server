#!/bin/sh
# 抓 SFU 上 40100-40131 的通话包。安静数秒后停止，并打印每路的编码号、包数和断号。
# 在仓库目录执行。本机没有 SFU 时，转到 cs 上抓，pcap 放到项目的 data/pcap 目录。
# RTP_IDLE_SECONDS 默认 5，RTP_WAIT_SECONDS 默认 600。
set -eu

idle=${RTP_IDLE_SECONDS:-5}
wait_first=${RTP_WAIT_SECONDS:-600}
umin=${RTP_UDP_MIN:-40100}
umax=${RTP_UDP_MAX:-40131}
remote=${RTP_SSH:-cs}

analyze() {
  python3 - "$1" "$umin" "$umax" <<'PY'
import collections, socket, struct, sys

path, umin, umax = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
data = open(path, "rb").read()
if len(data) < 24:
    raise SystemExit("pcap 是空的")
magic = data[:4]
if magic == b"\xd4\xc3\xb2\xa1":
    endian = "<"
elif magic == b"\xa1\xb2\xc3\xd4":
    endian = ">"
else:
    raise SystemExit("不是经典 pcap")
link = struct.unpack_from(endian + "I", data, 20)[0]

NAMES = {0: "PCMU/8000", 8: "PCMA/8000", 9: "G722/8000", 13: "CN/8000"}

def ipv4_udp(frame):
    if link == 1:
        if len(frame) < 14:
            return None
        eth = 12
        etype = struct.unpack_from("!H", frame, eth)[0]
        off = 14
        if etype == 0x8100 and len(frame) >= 18:
            etype = struct.unpack_from("!H", frame, 16)[0]
            off = 18
        if etype != 0x0800:
            return None
    elif link == 113:
        if len(frame) < 16:
            return None
        if struct.unpack_from("!H", frame, 14)[0] != 0x0800:
            return None
        off = 16
    elif link == 276:
        if len(frame) < 20:
            return None
        if struct.unpack_from("!H", frame, 0)[0] != 0x0800:
            return None
        off = 20
    elif link == 101:
        off = 0
    else:
        raise SystemExit("不认识的链路类型 %s" % link)
    if len(frame) < off + 20 or frame[off] >> 4 != 4 or frame[off + 9] != 17:
        return None
    ihl = (frame[off] & 0x0F) * 4
    u = off + ihl
    if len(frame) < u + 8:
        return None
    sp, dp, ln = struct.unpack_from("!HHH", frame, u)
    payload = frame[u + 8:u + ln]
    src = socket.inet_ntoa(frame[off + 12:off + 16])
    dst = socket.inet_ntoa(frame[off + 16:off + 20])
    return src, sp, dst, dp, payload

class Flow:
    def __init__(self):
        self.seqs = []

    def add(self, seq):
        self.seqs.append(seq)

    def report(self):
        packets = len(self.seqs)
        if packets == 0:
            return 0, 0, []
        ext = 0
        last = self.seqs[0]
        values = [last]
        for seq in self.seqs[1:]:
            cand = seq + ext
            while cand < last - 32768:
                ext += 65536
                cand = seq + ext
            while cand > last + 32768:
                cand -= 65536
            values.append(cand)
            if cand > last:
                last = cand
        ordered = sorted(set(values))
        duplicate = packets - len(ordered)
        missing = 0
        ranges = []
        for left, right in zip(ordered, ordered[1:]):
            if right <= left + 1:
                continue
            missing += right - left - 1
            if len(ranges) < 6:
                ranges.append("%d-%d" % ((left + 1) & 0xFFFF, (right - 1) & 0xFFFF)
                              if right > left + 2 else str((left + 1) & 0xFFFF))
        return duplicate, missing, ranges

flows = collections.OrderedDict()
off = 24
while off + 16 <= len(data):
    _ts, _us, incl, _orig = struct.unpack_from(endian + "IIII", data, off)
    off += 16
    if incl <= 0:
        break
    frame = data[off:off + incl]
    off += incl
    parsed = ipv4_udp(frame)
    if parsed is None:
        continue
    src, sp, dst, dp, payload = parsed
    if len(payload) < 12 or payload[0] >> 6 != 2:
        continue
    marker = payload[1]
    if 192 <= marker <= 223:
        continue
    pt = marker & 0x7F
    seq = struct.unpack_from("!H", payload, 2)[0]
    ssrc = struct.unpack_from("!I", payload, 8)[0]
    key = (src, sp, dst, dp, ssrc, pt)
    flow = flows.get(key)
    if flow is None:
        flow = Flow()
        flows[key] = flow
    flow.add(seq)

if not flows:
    print("没有 RTP")
    raise SystemExit(0)

for (src, sp, dst, dp, ssrc, pt), flow in flows.items():
    duplicate, missing, ranges = flow.report()
    name = NAMES.get(pt, "")
    codec = (" " + name) if name else ""
    if umin <= dp <= umax and not (umin <= sp <= umax):
        way = "上行"
    elif umin <= sp <= umax and not (umin <= dp <= umax):
        way = "下行"
    else:
        way = "本机"
    gap = "断号=0"
    if missing:
        shown = ",".join(ranges)
        gap = "断号=%d 缺 %s" % (missing, shown) if shown else "断号=%d" % missing
    extra = ""
    if duplicate:
        extra = " 重复=%d" % duplicate
    print("%s %s:%d -> %s:%d  pt=%d%s  ssrc=%d  包数=%d  %s%s" % (
        way, src, sp, dst, dp, pt, codec, ssrc, len(flow.seqs), gap, extra))
PY
}

if [ "${1:-}" = "--analyze" ]; then
  [ -n "${2:-}" ] || { echo "usage: $0 --analyze file.pcap" >&2; exit 1; }
  analyze "$2"
  exit 0
fi

on_sfu=0
if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q '127.0.0.1:18096'; then
  on_sfu=1
fi

if [ "${1:-}" != "--capture" ] && [ "$on_sfu" -ne 1 ]; then
  stamp=$(date +%Y%m%d-%H%M%S)
  name="collar-rtp-${stamp}.pcap"
  root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
  mkdir -p "${root}/data/pcap"
  echo "本机没有 SFU，改到 ${remote} 上抓"
  ssh -o BatchMode=yes "$remote" "sh -s -- --capture /tmp/${name}" < "$0"
  scp -o BatchMode=yes -q "${remote}:/tmp/${name}" "${root}/data/pcap/${name}"
  echo "pcap 已放在 data/pcap/${name}"
  exit 0
fi

if ! command -v tcpdump >/dev/null 2>&1; then
  echo "没有 tcpdump" >&2
  exit 1
fi

if [ "${1:-}" = "--capture" ]; then
  pcap=${2:?pcap path}
else
  stamp=$(date +%Y%m%d-%H%M%S)
  root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
  mkdir -p "${root}/data/pcap"
  pcap="${root}/data/pcap/collar-rtp-${stamp}.pcap"
fi

echo "开始抓 UDP ${umin}-${umax}。等通话包出现；安静 ${idle} 秒后停止。"
tcpdump -i any -n -s 0 -U -w "$pcap" "udp portrange ${umin}-${umax}" >/dev/null 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT INT TERM
sleep 1
if ! kill -0 "$pid" 2>/dev/null; then
  echo "tcpdump 没有跑起来" >&2
  exit 1
fi

start=$(date +%s)
last_change=$start
last_size=$(wc -c < "$pcap")
seen=0
while kill -0 "$pid" 2>/dev/null; do
  sleep 1
  size=$(wc -c < "$pcap")
  now=$(date +%s)
  if [ "$size" -gt "$last_size" ]; then
    if [ "$seen" -eq 0 ]; then
      echo "已经收到包，通话结束后会自动停"
    fi
    seen=1
    last_size=$size
    last_change=$now
  fi
  if [ "$seen" -eq 0 ] && [ $((now - start)) -ge "$wait_first" ]; then
    echo "等了 ${wait_first} 秒还没有包" >&2
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    trap - EXIT INT TERM
    exit 1
  fi
  if [ "$seen" -eq 1 ] && [ $((now - last_change)) -ge "$idle" ]; then
    kill "$pid" 2>/dev/null || true
    break
  fi
done
wait "$pid" 2>/dev/null || true
trap - EXIT INT TERM

echo "停止，开始看这一通"
analyze "$pcap"
if [ "${1:-}" != "--capture" ]; then
  echo "pcap 已放在 data/pcap/$(basename "$pcap")"
fi
