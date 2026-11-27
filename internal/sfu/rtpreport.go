package sfu

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RTPReport 是一次设备上行诊断的汇总。G722Confirmed 恒为 false：没有解码就不能认定编码。
type RTPReport struct {
	SessionID            string         `json:"session_id"`
	Dir                  string         `json:"dir,omitempty"`
	Running              bool           `json:"running"`
	Reason               string         `json:"reason,omitempty"`
	Packets              int            `json:"packets"`
	Dropped              int            `json:"dropped"`
	Targets              int            `json:"targets"`
	PayloadType          map[string]int `json:"payload_type"`
	PayloadLengthMin     int            `json:"payload_length_min"`
	PayloadLengthMax     int            `json:"payload_length_max"`
	PayloadLengthAvg     float64        `json:"payload_length_avg"`
	Loss                 int            `json:"loss"`
	Duplicate            int            `json:"duplicate"`
	Reorder              int            `json:"reorder"`
	TimestampDelta       map[string]int `json:"timestamp_delta"`
	TimestampStep160     int            `json:"timestamp_step_160"`
	TimestampSteps       int            `json:"timestamp_steps"`
	Identical            int            `json:"identical"`
	PayloadChanged       int            `json:"payload_changed"`
	PayloadLengthChanged int            `json:"payload_length_changed"`
	MissingOutbound      int            `json:"missing_outbound"`
	HeaderRewritten      int            `json:"header_rewritten"`
	MatchRate            float64        `json:"match_rate"`
	PTIs9                bool           `json:"pt_is_9"`
	TimestampMatchesG722 bool           `json:"timestamp_matches_g722"`
	PayloadIdentical     bool           `json:"payload_identical"`
	ForwardComplete      bool           `json:"forward_complete"`
	G722Confirmed        bool           `json:"g722_confirmed"`
	Text                 string         `json:"text"`
}

type debugPacket struct {
	ID        uint64
	Source    string
	TrackID   string
	PT        uint8
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Marker    bool
	Length    int
	Hash      string
	Prefix    string
	Out       []debugOut
}

type debugOut struct {
	Target    string
	PT        uint8
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Marker    bool
	Length    int
	Hash      string
	OK        bool
}

type comparison struct {
	PacketID  uint64
	Target    string
	Verdict   string
	Rewritten bool
}

func comparisons(sample debugPacket) []comparison {
	outs := sample.Out
	if len(outs) == 0 {
		outs = []debugOut{{OK: false}}
	}
	out := make([]comparison, 0, len(outs))
	for _, item := range outs {
		verdict, rewritten := classify(sample, item)
		out = append(out, comparison{PacketID: sample.ID, Target: item.Target, Verdict: verdict, Rewritten: rewritten})
	}
	return out
}

// classify 只按 Payload 下结论。头被改写时 Rewritten 为 true，但哈希相同仍是 IDENTICAL。
func classify(in debugPacket, out debugOut) (string, bool) {
	rewritten := in.PT != out.PT || in.SSRC != out.SSRC || in.Seq != out.Seq || in.Timestamp != out.Timestamp || in.Marker != out.Marker
	if !out.OK || out.Target == "" {
		return "MISSING_OUTBOUND", rewritten
	}
	if in.Length != out.Length {
		return "PAYLOAD_LENGTH_CHANGED", rewritten
	}
	if in.Hash != out.Hash {
		return "PAYLOAD_CHANGED", rewritten
	}
	return "IDENTICAL", rewritten
}

func buildReport(sessionID, dir, reason string, running bool, dropped int, samples []debugPacket) RTPReport {
	report := RTPReport{
		SessionID:      sessionID,
		Dir:            dir,
		Running:        running,
		Reason:         reason,
		Packets:        len(samples),
		Dropped:        dropped,
		PayloadType:    map[string]int{},
		TimestampDelta: map[string]int{},
	}
	if len(samples) == 0 {
		report.Text = explain(report)
		return report
	}
	sum := 0
	report.PayloadLengthMin = samples[0].Length
	targets := map[string]struct{}{}
	for _, sample := range samples {
		report.PayloadType[strconv.Itoa(int(sample.PT))]++
		sum += sample.Length
		if sample.Length < report.PayloadLengthMin {
			report.PayloadLengthMin = sample.Length
		}
		if sample.Length > report.PayloadLengthMax {
			report.PayloadLengthMax = sample.Length
		}
		for _, item := range comparisons(sample) {
			switch item.Verdict {
			case "IDENTICAL":
				report.Identical++
				if item.Rewritten {
					report.HeaderRewritten++
				}
			case "PAYLOAD_CHANGED":
				report.PayloadChanged++
			case "PAYLOAD_LENGTH_CHANGED":
				report.PayloadLengthChanged++
			default:
				report.MissingOutbound++
			}
			if item.Target != "" {
				targets[item.Target] = struct{}{}
			}
		}
	}
	report.Targets = len(targets)
	report.PayloadLengthAvg = float64(sum) / float64(len(samples))
	report.PTIs9 = len(report.PayloadType) == 1 && report.PayloadType["9"] == len(samples)
	report.Loss, report.Duplicate, report.Reorder = sequenceStats(samples)
	report.TimestampDelta, report.TimestampSteps, report.TimestampStep160 = timestampDeltas(samples)
	if report.TimestampSteps > 0 && report.TimestampStep160*10 >= report.TimestampSteps*9 {
		report.TimestampMatchesG722 = true
	}
	total := report.Identical + report.PayloadChanged + report.PayloadLengthChanged + report.MissingOutbound
	if total > 0 {
		report.MatchRate = float64(report.Identical) / float64(total)
	}
	report.PayloadIdentical = total > 0 && report.PayloadChanged == 0 && report.PayloadLengthChanged == 0 && report.MissingOutbound == 0
	report.ForwardComplete = report.PayloadIdentical && report.Loss == 0
	report.Text = explain(report)
	return report
}

func sequenceStats(samples []debugPacket) (loss, duplicate, reorder int) {
	var prev int64
	var has bool
	var maxSeen int64
	seen := map[int64]int{}
	var order []int64
	for _, sample := range samples {
		ext := int64(sample.Seq)
		if has {
			ext = prev + int64(int16(sample.Seq-uint16(prev)))
		}
		prev = ext
		has = true
		seen[ext]++
		if seen[ext] > 1 {
			duplicate++
			continue
		}
		if len(order) > 0 && ext < maxSeen {
			reorder++
		}
		if len(order) == 0 || ext > maxSeen {
			maxSeen = ext
		}
		order = append(order, ext)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for i := 1; i < len(order); i++ {
		gap := order[i] - order[i-1] - 1
		if gap > 0 {
			loss += int(gap)
		}
	}
	return loss, duplicate, reorder
}

func timestampDeltas(samples []debugPacket) (map[string]int, int, int) {
	var prev int64
	var has bool
	seen := map[int64]struct{}{}
	type stamp struct {
		ext int64
		ts  uint32
	}
	var stamps []stamp
	for _, sample := range samples {
		ext := int64(sample.Seq)
		if has {
			ext = prev + int64(int16(sample.Seq-uint16(prev)))
		}
		prev = ext
		has = true
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		stamps = append(stamps, stamp{ext: ext, ts: sample.Timestamp})
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].ext < stamps[j].ext })
	dist := map[string]int{}
	steps := 0
	step160 := 0
	for i := 1; i < len(stamps); i++ {
		delta := int32(stamps[i].ts - stamps[i-1].ts)
		dist[strconv.FormatInt(int64(delta), 10)]++
		steps++
		if delta == 160 {
			step160++
		}
	}
	return dist, steps, step160
}

func explain(report RTPReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "会话 %s\n", report.SessionID)
	if report.Running {
		b.WriteString("状态：正在记录设备上行。\n")
	} else {
		fmt.Fprintf(&b, "状态：已停止（%s）。\n", reasonText(report.Reason))
	}
	fmt.Fprintf(&b, "包数：%d。调试队列丢弃：%d。丢弃的只是调试记录，转发没有停。\n", report.Packets, report.Dropped)
	fmt.Fprintf(&b, "转发目标数：%d。\n", report.Targets)
	if report.Packets == 0 {
		b.WriteString("PT是否为9：还没有设备上行包。\n")
		b.WriteString("时间戳：包数不够，看不出增量。G.722/8000 的 20ms 帧应增加 160，也就是 8000×0.02。\n")
		b.WriteString("Payload长度：没有样本。640 字节约是编码前 20ms 的 PCM，不是 G.722 包的固定长度。\n")
	} else if report.PTIs9 {
		fmt.Fprintf(&b, "PT是否为9：是，%d 个包的 Payload Type 都是 9。\n", report.Packets)
	} else {
		fmt.Fprintf(&b, "PT是否为9：否，分布是 %s。\n", formatCounts(report.PayloadType))
	}
	if report.Packets > 0 && report.TimestampSteps == 0 {
		b.WriteString("时间戳：只有一个包，看不出增量。G.722/8000 的 20ms 帧应增加 160。\n")
	} else if report.TimestampSteps > 0 && report.TimestampMatchesG722 {
		fmt.Fprintf(&b, "时间戳：符合。%d 次相邻增量里 %d 次是 160，这是 8000×0.02。\n", report.TimestampSteps, report.TimestampStep160)
	} else if report.TimestampSteps > 0 {
		fmt.Fprintf(&b, "时间戳：不符合。%d 次相邻增量里只有 %d 次是 160。分布 %s。\n", report.TimestampSteps, report.TimestampStep160, formatCounts(report.TimestampDelta))
	}
	if report.Packets > 0 {
		fmt.Fprintf(&b, "Payload长度：最小 %d，最大 %d，平均 %.1f。640 字节约是编码前 20ms 的 PCM，不是 G.722 包的固定长度。\n", report.PayloadLengthMin, report.PayloadLengthMax, report.PayloadLengthAvg)
	}
	fmt.Fprintf(&b, "序号：丢包 %d，乱序 %d，重复 %d。\n", report.Loss, report.Reorder, report.Duplicate)
	total := report.Identical + report.PayloadChanged + report.PayloadLengthChanged + report.MissingOutbound
	if total == 0 {
		b.WriteString("Payload对比：没有出站记录。\n")
	} else {
		fmt.Fprintf(&b, "Payload对比：%d 次里 %d 次 IDENTICAL，一致率 %.0f%%。PAYLOAD_CHANGED %d，PAYLOAD_LENGTH_CHANGED %d，MISSING_OUTBOUND %d。头被改写但 Payload 未变 %d。\n",
			total, report.Identical, report.MatchRate*100, report.PayloadChanged, report.PayloadLengthChanged, report.MissingOutbound, report.HeaderRewritten)
	}
	switch {
	case report.Packets == 0:
		b.WriteString("转发是否完整：还没有设备上行包。\n")
	case report.ForwardComplete:
		b.WriteString("转发是否完整：SFU 交给 App 的 Payload 与入站一致，序号没有缺口。这只说明服务器发出的内容完整。\n")
	default:
		b.WriteString("转发是否完整：不完整。看丢包和 MISSING_OUTBOUND。\n")
	}
	b.WriteString("是不是G.722：不能。PT=9 和时间戳增量 160 只说明封包像 G.722/8000 的 20ms 音频，Payload 没有解码，不能据此确认编码。\n")
	return b.String()
}

func reasonText(reason string) string {
	switch reason {
	case "time":
		return "到了设定的秒数"
	case "packets":
		return "到了包数上限"
	case "bytes":
		return "到了文件大小上限"
	case "stop":
		return "手动停止"
	case "room_closed":
		return "房间关闭"
	case "write":
		return "写文件失败"
	default:
		if reason == "" {
			return "停止"
		}
		return reason
	}
}

func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(parts, " ")
}
