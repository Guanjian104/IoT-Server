// Package bridge 维持服务端 MQTT 连接、上行处理和命令发送。
package bridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"iot-server/internal/config"
	"iot-server/internal/logger"
	"iot-server/internal/metrics"
	"iot-server/internal/mqttmsg"
	"iot-server/internal/store"
)

// Bridge 是一个服务账号连接。业务结果先写入 MySQL，再确认 MQTT。
type Bridge struct {
	client     mqtt.Client
	store      *store.Store
	log        logger.Logger
	metrics    *metrics.Metrics
	tenant     string
	ctx        context.Context
	cancel     context.CancelFunc
	once       sync.Once
	lane       *lane
	sessions   sessionSource
	subscribed atomic.Bool
}

// sessionSource 列出 Broker 上仍连接的客户端。时间为毫秒；第二个结果表示列表已经扫完。
type sessionSource interface {
	ConnectedDevices(ctx context.Context) (map[string]int64, bool, error)
}

const sessionCalibrateEvery = 5 * time.Second

// New 创建连接，但要等数据库迁移完成后再 Ensure。
func New(parent context.Context, cfg config.Config, st *store.Store, log logger.Logger, m *metrics.Metrics) *Bridge {
	ctx, cancel := context.WithCancel(parent)
	b := &Bridge{store: st, log: log, metrics: m, tenant: cfg.TenantID, ctx: ctx, cancel: cancel, lane: newLane()}
	b.lane.isolate = b.isolateInbox
	opts := serviceOptions(cfg)
	opts.SetDefaultPublishHandler(b.onMessage)
	opts.SetOnConnectHandler(b.onConnect)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, _ error) {
		b.subscribed.Store(false)
		if b.log != nil {
			b.log.Warn("mqtt connection lost", "instance_id", cfg.InstanceID)
		}
	})
	b.client = mqtt.NewClient(opts)
	return b
}

// UseSessions 接入 Broker 会话列表。未调用时不做在线校正。需要在 Ensure 之前调用。
func (b *Bridge) UseSessions(src sessionSource) {
	if b != nil {
		b.sessions = src
	}
}

// serviceOptions 使用持久会话。未确认的 QoS 1 报文在断线和进程重启后由 Broker 重投。
// 客户端 ID 来自实例标识，实例 ID 变化会丢掉上一份会话。业务写入完成前不确认。
func serviceOptions(cfg config.Config) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTTBrokerURL)
	opts.SetClientID(mqttmsg.ServiceClientID(cfg.InstanceID))
	opts.SetUsername(cfg.MQTTServiceUser)
	opts.SetPassword(cfg.MQTTServicePass)
	opts.SetProtocolVersion(4)
	opts.SetCleanSession(false)
	opts.SetResumeSubs(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(2 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetWriteTimeout(5 * time.Second)
	opts.SetOrderMatters(false)
	opts.SetAutoAckDisabled(true)
	return opts
}

// Ensure 启动发送循环和 Broker 连接。重复调用只生效一次。
func (b *Bridge) Ensure() {
	if b == nil || b.client == nil {
		return
	}
	b.once.Do(func() {
		b.resumeInbox()
		go b.lane.run(b.ctx, b.handle, 200*time.Millisecond, func(err error, first bool) {
			if first && errors.Is(err, store.ErrUnavailable) && b.log != nil {
				b.log.Error("mqtt dispatch failed")
			}
		})
		go b.reconcilePoison()
		go b.calibratePresence()
		go b.outbox()
		token := b.client.Connect()
		go func() {
			_ = token.WaitTimeout(10 * time.Second)
		}()
	})
}

// Connected 报告当前这条服务连接是否可用。
func (b *Bridge) Connected() bool {
	return b != nil && b.client != nil && b.client.IsConnected()
}

// Subscribed 报告服务连接是否已经订完业务过滤器和会话事件。
func (b *Bridge) Subscribed() bool {
	return b != nil && b.subscribed.Load()
}

// Ready 报告连接已经建立并且订阅已经完成。
func (b *Bridge) Ready() bool {
	return b.Connected() && b.Subscribed()
}

// Close 停止发送循环并断开连接。
func (b *Bridge) Close() {
	if b == nil {
		return
	}
	b.cancel()
	if b.client != nil && b.client.IsConnected() {
		b.client.Disconnect(250)
	}
}

func (b *Bridge) onConnect(client mqtt.Client) {
	b.subscribed.Store(false)
	filters := map[string]byte{}
	for _, filter := range mqttmsg.ServiceFilters() {
		filters[filter] = 1
	}
	token := client.SubscribeMultiple(filters, nil)
	waited := token.WaitTimeout(5 * time.Second)
	if !waited || token.Error() != nil {
		b.subscribed.Store(false)
		if b.log != nil {
			reason := "timeout"
			if token.Error() != nil {
				reason = token.Error().Error()
			}
			b.log.Error("mqtt subscribe failed", "reason", reason)
		}
		go client.Disconnect(0)
		return
	}
	b.subscribed.Store(true)
	if b.log != nil {
		b.log.Info("mqtt subscribed")
	}
}

func (b *Bridge) onMessage(_ mqtt.Client, msg mqtt.Message) {
	// 数据库失败时留在有界队列里重试，成功后才确认。队列满了不确认，持久会话会重投。
	body := append([]byte(nil), msg.Payload()...)
	if !b.lane.add(delivery{topic: msg.Topic(), body: body, ack: msg.Ack}) && b.log != nil {
		b.log.Warn("mqtt inbound full")
	}
}

func (b *Bridge) handle(topic string, payload []byte) error {
	if len(payload) > 64*1024 {
		deviceID, _, _ := mqttmsg.SplitTopic(topic)
		if err := b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "too_large"); err != nil {
			return err
		}
		b.note()
		return nil
	}
	done, err := b.store.StageInbox(b.ctx, b.tenant, topic, payload)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if err := b.dispatch(topic, payload); err != nil {
		return err
	}
	return b.store.CompleteInbox(b.ctx, b.tenant, topic, payload)
}

func (b *Bridge) resumeInbox() {
	pending, err := b.store.PendingInbox(b.ctx, b.tenant, laneCap)
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("mqtt inbox resume failed")
		}
		return
	}
	for _, item := range pending {
		if !b.lane.add(delivery{topic: item.Topic, body: append([]byte(nil), item.Payload...)}) && b.log != nil {
			b.log.Warn("mqtt inbound full")
			return
		}
	}
}

func (b *Bridge) isolateInbox(item delivery) bool {
	reason := "poison"
	if err := b.store.PoisonInbox(b.ctx, b.tenant, item.topic, item.body, reason); err != nil {
		return false
	}
	return true
}

func (b *Bridge) reconcilePoison() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.retryPoison()
		}
	}
}

func (b *Bridge) calibratePresence() {
	if b.sessions == nil || b.store == nil {
		return
	}
	ticker := time.NewTicker(sessionCalibrateEvery)
	defer ticker.Stop()
	for {
		b.calibrateOnce()
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) calibrateOnce() {
	ctx, cancel := context.WithTimeout(b.ctx, 4*time.Second)
	defer cancel()
	online, complete, err := b.sessions.ConnectedDevices(ctx)
	if err != nil || ctx.Err() != nil {
		return
	}
	if err := b.store.CalibratePresence(ctx, b.tenant, online, complete); err != nil && b.ctx.Err() == nil && b.log != nil {
		b.log.Error("presence calibration failed")
	}
}

func (b *Bridge) retryPoison() {
	batch, err := b.store.PoisonInboxBatch(b.ctx, b.tenant, 4)
	if err != nil {
		return
	}
	for _, item := range batch {
		err := b.handle(item.Topic, item.Payload)
		if err == nil {
			continue
		}
		if errors.Is(err, store.ErrUnavailable) {
			return
		}
		reason := "poison"
		if err.Error() == "waiting" {
			reason = "waiting"
		}
		if noteErr := b.store.NotePoisonInbox(b.ctx, b.tenant, item.Topic, item.Payload, reason); noteErr != nil {
			return
		}
	}
	_ = b.store.SweepInbox(b.ctx, b.tenant)
}

func (b *Bridge) deviceOpen(deviceID string) (bool, error) {
	gate, err := b.store.DeviceGate(b.ctx, b.tenant, deviceID)
	if err != nil {
		return false, err
	}
	return gate.Found && gate.Enabled && !gate.Revoked, nil
}

func (b *Bridge) note() {
	if b.metrics != nil {
		b.metrics.NoteInbound()
	}
}

func (b *Bridge) dispatch(topic string, payload []byte) error {
	if strings.HasPrefix(topic, "$SYS/") {
		deviceID, event, ok := ParseSys(topic, payload)
		if !ok {
			return nil
		}
		return b.store.ApplySession(b.ctx, b.tenant, deviceID, event)
	}
	deviceID, kind, err := mqttmsg.SplitTopic(topic)
	if err != nil {
		return b.store.RejectInbound(b.ctx, b.tenant, "", topic, "topic")
	}
	open, err := b.deviceOpen(deviceID)
	if err != nil {
		return err
	}
	if !open {
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "device")
	}
	switch kind {
	case "state":
		return b.ingestState(deviceID, topic, payload)
	default:
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "topic")
	}
}

func (b *Bridge) ingestState(deviceID, topic string, payload []byte) error {
	message, err := mqttmsg.ParseState(topic, payload)
	if err != nil {
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "state")
	}
	switch message.Kind {
	case "status":
		b.note()
		return b.store.SetReportedStatus(b.ctx, b.tenant, deviceID, message.Status)
	case "cmd_result":
		return b.ingestReply(deviceID, message.Reply)
	case "report":
		return b.ingestReport(deviceID, message.Report, payload)
	default:
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "state")
	}
}

func (b *Bridge) ingestReport(deviceID string, report mqttmsg.Report, payload []byte) error {
	report.MsgID = mqttmsg.Hash(payload)
	duplicate, err := b.store.IngestReport(b.ctx, b.tenant, report, payload)
	if err != nil {
		return err
	}
	b.note()
	if b.log != nil {
		b.log.Info("device state", "device_id", deviceID, "duplicate", duplicate)
	}
	return nil
}

func (b *Bridge) ingestReply(deviceID string, reply mqttmsg.CmdResult) error {
	var result string
	var err error
	for i := 0; i < 10; i++ {
		result, err = b.store.ApplyReply(b.ctx, b.tenant, deviceID, reply.Cmd, reply.Result, reply.Code)
		if err != nil {
			return err
		}
		if result != "waiting" {
			break
		}
		select {
		case <-b.ctx.Done():
			return errors.New("waiting")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if result == "waiting" {
		return errors.New("waiting")
	}
	b.note()
	if b.log != nil {
		b.log.Info("command reply", "device_id", deviceID, "cmd", reply.Cmd, "result", result, "reply_code", reply.Code)
	}
	return nil
}

func (b *Bridge) outbox() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		b.pass()
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) pass() {
	if err := b.maintain(); err != nil {
		return
	}
	if b.client == nil || !b.client.IsConnected() || b.ctx.Err() != nil {
		return
	}
	items, err := b.pull()
	if err != nil {
		return
	}
	for i, item := range items {
		if b.ctx.Err() != nil {
			b.releaseUnsent(items[i:])
			return
		}
		b.publishOne(item)
	}
}

func (b *Bridge) maintain() error {
	ctx, cancel := context.WithTimeout(b.ctx, commandScanTimeout)
	defer cancel()
	timedOut, err := b.store.Maintain(ctx, b.tenant)
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("command maintain failed")
		}
		return err
	}
	if timedOut > 0 {
		if b.metrics != nil {
			b.metrics.NoteCommandTimeout(timedOut)
		}
		if b.log != nil {
			b.log.Info("commands timed out", "count", timedOut)
		}
	}
	return nil
}

func (b *Bridge) pull() ([]store.Outbound, error) {
	ctx, cancel := context.WithTimeout(b.ctx, commandScanTimeout)
	defer cancel()
	items, err := b.store.PullPending(ctx, b.tenant, 8)
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("command pull failed")
		}
		return nil, err
	}
	return items, nil
}

func (b *Bridge) publishOne(item store.Outbound) {
	screenCtx, cancel := context.WithTimeout(b.ctx, commandScanTimeout)
	action, err := b.store.PrepareSend(screenCtx, item.ID)
	cancel()
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("command prepare failed", "command_id", item.ID, "device_id", item.DeviceID)
		}
		return
	}
	if action != "send" {
		return
	}
	if !mqttmsg.ValidDeviceID(item.DeviceID) {
		b.recordPublish(item, false, errInvalidDevice)
		return
	}
	wait, ok := ackWait(item.Deadline, time.Now())
	if !ok {
		ctx, cancel := persistContext(b.ctx)
		defer cancel()
		_ = b.store.ConcludeSending(ctx, item.ID, "timeout")
		return
	}
	// QoS 1 只保证至少送达一次。设备报文没有 msgId，相同载荷再次到达会被再执行。
	token := b.client.Publish(mqttmsg.DeviceTopic(item.DeviceID, "cmd"), 1, false, item.Payload)
	uncertain := !token.WaitTimeout(wait)
	if uncertain || token.Error() != nil {
		b.recordPublish(item, uncertain || token.Error() == nil, token.Error())
		return
	}
	ctx, cancel := persistContext(b.ctx)
	defer cancel()
	if err := b.store.MarkSent(ctx, item.ID); err != nil && b.log != nil {
		b.log.Error("command mark sent failed", "command_id", item.ID, "device_id", item.DeviceID)
	} else if b.log != nil {
		b.log.Info("command sent", "command_id", item.ID, "device_id", item.DeviceID)
	}
}

var errInvalidDevice = errors.New("invalid device")

func (b *Bridge) recordPublish(item store.Outbound, uncertain bool, cause error) {
	ctx, cancel := persistContext(b.ctx)
	defer cancel()
	attempts := item.Attempts
	if errors.Is(cause, errInvalidDevice) {
		attempts = 3
		uncertain = false
	}
	_ = b.store.MarkPublishResult(ctx, item.ID, attempts, uncertain)
	if b.log != nil && !errors.Is(cause, errInvalidDevice) {
		b.log.Warn("command publish failed", "command_id", item.ID, "device_id", item.DeviceID)
	}
}

const (
	commandScanTimeout = 2 * time.Second
	publishAckWait     = 3 * time.Second
	resultWriteTimeout = 2 * time.Second
	minSendWindow      = 200 * time.Millisecond
)

// persistContext 给落库单独留时间。发布等待不再占用这条上下文，慢 Broker 不能把结果写丢掉。
func persistContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), resultWriteTimeout)
}

// ackWait 把单条确认等待限制在截止时间之内。剩余窗口太短时不再发布。
func ackWait(deadline, now time.Time) (time.Duration, bool) {
	until := deadline.Sub(now)
	if until < minSendWindow {
		return 0, false
	}
	if until < publishAckWait {
		return until, true
	}
	return publishAckWait, true
}

func (b *Bridge) releaseUnsent(items []store.Outbound) {
	ctx, cancel := persistContext(b.ctx)
	defer cancel()
	for _, item := range items {
		if err := b.store.ReleaseUnsent(ctx, item.ID, item.Attempts); err != nil && b.log != nil {
			b.log.Error("command release failed", "command_id", item.ID, "device_id", item.DeviceID)
		}
	}
}
