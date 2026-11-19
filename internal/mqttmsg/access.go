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

var (
	cmdTopic   = regexp.MustCompile(`^pet/collar/1_0pro/([0-9a-f]{12})/cmd$`)
	stateTopic = regexp.MustCompile(`^pet/collar/1_0pro/([0-9a-f]{12})/state$`)
)

// SplitTopic 解析项圈业务主题。kind 为 cmd 或 state。
func SplitTopic(topic string) (deviceID, kind string, err error) {
	switch {
	case cmdTopic.MatchString(topic):
		return cmdTopic.FindStringSubmatch(topic)[1], "cmd", nil
	case stateTopic.MatchString(topic):
		return stateTopic.FindStringSubmatch(topic)[1], "state", nil
	default:
		return "", "", errTopic
	}
}

var errTopic = topicError{}

type topicError struct{}

func (topicError) Error() string { return "invalid topic" }

// DeviceTopic 拼出某个设备的业务主题。
func DeviceTopic(deviceID, kind string) string {
	switch kind {
	case "cmd":
		return "pet/collar/1_0pro/" + deviceID + "/cmd"
	case "state":
		return "pet/collar/1_0pro/" + deviceID + "/state"
	default:
		return ""
	}
}

// Decide 判断一个已经区分过身份的主体能否发布或订阅该主题。
func Decide(kind, deviceID, action, topic string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	switch kind {
	case "device":
		return decideDevice(deviceID, action, topic)
	case "service":
		return decideService(action, topic)
	default:
		return "deny"
	}
}

func decideDevice(deviceID, action, topic string) string {
	if deviceID == "" || strings.ContainsAny(topic, "#+") {
		return "deny"
	}
	switch action {
	case "publish":
		id, kind, err := SplitTopic(topic)
		if err != nil || id != deviceID || kind == "cmd" {
			return "deny"
		}
		return "allow"
	case "subscribe":
		if topic == DeviceTopic(deviceID, "cmd") {
			return "allow"
		}
		return "deny"
	default:
		return "deny"
	}
}

func decideService(action, topic string) string {
	switch action {
	case "publish":
		if _, kind, err := SplitTopic(topic); err == nil && kind == "cmd" {
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
		"$share/" + ShareGroup + "/pet/collar/1_0pro/+/state",
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
