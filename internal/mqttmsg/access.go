package mqttmsg

import (
	"crypto/sha256"
	"crypto/subtle"
	"regexp"
	"strings"
)

const (
	ServiceUsername = "iot-dev-service"
	DeviceUsername  = "root"
	ShareGroup      = "iot-dev"
)

// Classify 区分服务账号、设备共享账号和尚未实现的 APP 账号。
func Classify(username, clientID string) (kind, deviceID string) {
	switch {
	case username == ServiceUsername && strings.HasPrefix(clientID, ServiceUsername+"-") && !ValidDeviceID(clientID):
		return "service", ""
	case username == DeviceUsername && ValidDeviceID(clientID):
		return "device", clientID
	case strings.HasPrefix(username, "app:"):
		return "app", ""
	default:
		return "unknown", ""
	}
}

// SameSecret 以固定长度摘要比较两个密钥，避免把明文写进日志。
func SameSecret(got, want string) bool {
	if want == "" {
		return false
	}
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// ServiceClientID 把实例标识收成 Broker 可接受的客户端 ID。
func ServiceClientID(instance string) string {
	var b strings.Builder
	b.WriteString(ServiceUsername)
	b.WriteByte('-')
	for _, r := range instance {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 96 {
			break
		}
	}
	return b.String()
}

const (
	Model10Pro  = "1_0pro"
	Model20Test = "2_0_test"
	ModelWatch  = "watch"
)

var (
	businessTopic = regexp.MustCompile(`^pet/collar/(1_0pro|2_0_test)/([0-9a-f]{12})/(cmd|state)$`)
	watchState    = regexp.MustCompile(`^pet/collar/(1_0pro|2_0_test)/([0-9a-f]{12}|\+)/state$`)
)

// KnownModel 判断主题代次是否已接入。watch 只用来订阅状态，不对应设备报文。
func KnownModel(model string) bool {
	return model == Model10Pro || model == Model20Test || model == ModelWatch
}

// SplitTopic 解析项圈业务主题。kind 为 cmd 或 state。
func SplitTopic(topic string) (deviceID, model, kind string, err error) {
	match := businessTopic.FindStringSubmatch(topic)
	if match == nil {
		return "", "", "", errTopic
	}
	return match[2], match[1], match[3], nil
}

var errTopic = topicError{}

type topicError struct{}

func (topicError) Error() string { return "invalid topic" }

// DeviceTopic 拼出 1_0pro 设备的业务主题。
func DeviceTopic(deviceID, kind string) string {
	return Topic(Model10Pro, deviceID, kind)
}

// Topic 按设备登记的代次拼出业务主题。
func Topic(model, deviceID, kind string) string {
	if (model != Model10Pro && model != Model20Test) || !ValidDeviceID(deviceID) {
		return ""
	}
	switch kind {
	case "cmd", "state":
		return "pet/collar/" + model + "/" + deviceID + "/" + kind
	default:
		return ""
	}
}

// Decide 判断一个已经区分过身份的主体能否发布或订阅该主题。
// model 是这台设备登记的主题代次；服务账号不使用它。
func Decide(kind, deviceID, model, action, topic string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	switch kind {
	case "device":
		return decideDevice(deviceID, model, action, topic)
	case "service":
		return decideService(action, topic)
	default:
		return "deny"
	}
}

func decideDevice(deviceID, model, action, topic string) string {
	if deviceID == "" || !KnownModel(model) {
		return "deny"
	}
	if model == ModelWatch {
		return decideWatch(action, topic)
	}
	if strings.ContainsAny(topic, "#+") {
		return "deny"
	}
	switch action {
	case "publish":
		id, got, kind, err := SplitTopic(topic)
		if err != nil || id != deviceID || got != model || kind != "state" {
			return "deny"
		}
		return "allow"
	case "subscribe":
		if topic == Topic(model, deviceID, "cmd") {
			return "allow"
		}
		return "deny"
	default:
		return "deny"
	}
}

// decideWatch 允许观察连接订阅状态原文。不能发布，也不能订控制指令。
func decideWatch(action, topic string) string {
	if action != "subscribe" || strings.Contains(topic, "#") {
		return "deny"
	}
	if watchState.MatchString(topic) {
		return "allow"
	}
	return "deny"
}

func decideService(action, topic string) string {
	switch action {
	case "publish":
		if _, _, kind, err := SplitTopic(topic); err == nil && kind == "cmd" {
			return "allow"
		}
		return "deny"
	case "subscribe":
		if serviceFilter(topic) {
			return "allow"
		}
		return "deny"
	default:
		return "deny"
	}
}

// ServiceFilters 是服务连接需要订阅的过滤器。设备业务主题使用共享订阅。
func ServiceFilters() []string {
	return []string{
		"$share/" + ShareGroup + "/pet/collar/" + Model10Pro + "/+/state",
		"$share/" + ShareGroup + "/pet/collar/" + Model20Test + "/+/state",
		"$SYS/brokers/+/clients/+/connected",
		"$SYS/brokers/+/clients/+/disconnected",
	}
}

func serviceFilter(topic string) bool {
	for _, filter := range ServiceFilters() {
		if topic == filter || topic == strings.TrimPrefix(filter, "$share/"+ShareGroup+"/") {
			return true
		}
	}
	return false
}
