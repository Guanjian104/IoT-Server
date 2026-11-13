// Package bridge 维持服务端 MQTT 连接、上行处理和命令发送。
package bridge

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	client  mqtt.Client
	store   *store.Store
	log     logger.Logger
	metrics *metrics.Metrics
	tenant  string
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	lane    *lane
}

// New 创建连接，但要等数据库迁移完成后再 Ensure。
func New(parent context.Context, cfg config.Config, st *store.Store, log logger.Logger, m *metrics.Metrics) *Bridge {
	ctx, cancel := context.WithCancel(parent)
	b := &Bridge{store: st, log: log, metrics: m, tenant: cfg.TenantID, ctx: ctx, cancel: cancel, lane: newLane()}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTTBrokerURL)
	opts.SetClientID(mqttmsg.ServiceClientID(cfg.InstanceID))
	opts.SetUsername(cfg.MQTTServiceUser)
	opts.SetPassword(cfg.MQTTServicePass)
	opts.SetProtocolVersion(4)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(2 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetWriteTimeout(5 * time.Second)
	opts.SetOrderMatters(false)
	opts.SetAutoAckDisabled(true)
	opts.SetDefaultPublishHandler(b.onMessage)
	opts.SetOnConnectHandler(b.onConnect)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, _ error) {
		if b.log != nil {
			b.log.Warn("mqtt connection lost", "instance_id", cfg.InstanceID)
		}
	})
	b.client = mqtt.NewClient(opts)
	return b
}

// Ensure 启动发送循环和 Broker 连接。重复调用只生效一次。
func (b *Bridge) Ensure() {
	if b == nil || b.client == nil {
		return
	}
	b.once.Do(func() {
		go b.lane.run(b.ctx, b.handle, 200*time.Millisecond, func(err error, first bool) {
			if first && errors.Is(err, store.ErrUnavailable) && b.log != nil {
				b.log.Error("mqtt dispatch failed")
			}
		})
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
	filters := map[string]byte{}
	for _, filter := range mqttmsg.ServiceFilters() {
		filters[filter] = 1
	}
	token := client.SubscribeMultiple(filters, nil)
	waited := token.WaitTimeout(5 * time.Second)
	if !waited || token.Error() != nil {
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
	if b.log != nil {
		b.log.Info("mqtt subscribed")
	}
}

func (b *Bridge) onMessage(_ mqtt.Client, msg mqtt.Message) {
	// 数据库失败时留在队列里重试，成功后才确认。不调用 Disconnect，否则自动重连会停掉。
	body := append([]byte(nil), msg.Payload()...)
	b.lane.add(delivery{topic: msg.Topic(), body: body, ack: msg.Ack})
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
	return b.dispatch(topic, payload)
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
	case "report":
		return b.ingestReport(deviceID, topic, payload)
	case "reply":
		return b.ingestReply(deviceID, topic, payload)
	case "status":
		status, err := mqttmsg.ParseStatus(topic, payload)
		if err != nil {
			b.note()
			return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "status")
		}
		b.note()
		return b.store.SetReportedStatus(b.ctx, b.tenant, deviceID, status)
	default:
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "topic")
	}
}

func (b *Bridge) ingestReport(deviceID, topic string, payload []byte) error {
	report, err := mqttmsg.ParseReport(topic, payload)
	if err != nil {
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "report")
	}
	duplicate, err := b.store.IngestReport(b.ctx, b.tenant, report, payload)
	if err != nil {
		return err
	}
	b.note()
	if b.log != nil {
		b.log.Info("property report", "device_id", deviceID, "msg_id", report.MsgID, "duplicate", duplicate)
	}
	return nil
}

func (b *Bridge) ingestReply(deviceID, topic string, payload []byte) error {
	reply, err := mqttmsg.ParseReply(topic, payload)
	if err != nil {
		b.note()
		return b.store.RejectInbound(b.ctx, b.tenant, deviceID, topic, "reply")
	}
	var result string
	for i := 0; i < 10; i++ {
		result, err = b.store.ApplyReply(b.ctx, b.tenant, deviceID, reply.MsgID, reply.Code)
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
		b.log.Info("command reply", "device_id", deviceID, "msg_id", reply.MsgID, "result", result, "reply_code", reply.Code)
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
	ctx, cancel := context.WithTimeout(b.ctx, 4*time.Second)
	defer cancel()
	timedOut, err := b.store.Maintain(ctx, b.tenant)
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("command maintain failed")
		}
		return
	}
	if timedOut > 0 {
		if b.metrics != nil {
			b.metrics.NoteCommandTimeout(timedOut)
		}
		if b.log != nil {
			b.log.Info("commands timed out", "count", timedOut)
		}
	}
	if !b.client.IsConnected() {
		return
	}
	items, err := b.store.PullPending(ctx, b.tenant, 8)
	if err != nil {
		if b.log != nil && b.ctx.Err() == nil {
			b.log.Error("command pull failed")
		}
		return
	}
	for _, item := range items {
		if !mqttmsg.ValidDeviceID(item.DeviceID) {
			_ = b.store.MarkPublishResult(ctx, item.ID, 3, false)
			continue
		}
		token := b.client.Publish(mqttmsg.DeviceTopic(item.DeviceID, "set"), 1, false, item.Payload)
		uncertain := !token.WaitTimeout(3 * time.Second)
		if uncertain || token.Error() != nil {
			_ = b.store.MarkPublishResult(ctx, item.ID, item.Attempts, uncertain || token.Error() == nil)
			if b.log != nil {
				b.log.Warn("command publish failed", "command_id", item.ID, "device_id", item.DeviceID)
			}
			continue
		}
		if err := b.store.MarkSent(ctx, item.ID); err != nil && b.log != nil {
			b.log.Error("command mark sent failed", "command_id", item.ID, "device_id", item.DeviceID)
		} else if b.log != nil {
			b.log.Info("command sent", "command_id", item.ID, "device_id", item.DeviceID)
		}
	}
}
