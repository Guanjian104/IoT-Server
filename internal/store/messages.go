package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"iot-server/internal/mqttmsg"
	"iot-server/internal/presence"
)

// IngestReport 先写去重键再写当前值和历史。相同报文返回 duplicate，相同 msgId 的新报文会保留。
func (s *Store) IngestReport(ctx context.Context, tenant string, report mqttmsg.Report, payload []byte) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, s.unavailable("ingest_report", err)
	}
	defer tx.Rollback()
	hash := mqttmsg.Hash(payload)
	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO inbound_dedup (tenant_id, device_id, msg_id, payload_hash)
		VALUES (?, ?, ?, ?)`, tenant, report.DeviceID, report.MsgID, hash)
	if err != nil {
		return false, s.unavailable("ingest_report", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, s.unavailable("ingest_report", err)
	}
	if n == 0 {
		if err := tx.Commit(); err != nil {
			return false, s.unavailable("ingest_report", err)
		}
		return true, nil
	}
	for _, field := range report.Fields {
		raw, err := fieldValue(field)
		if err != nil {
			return false, s.unavailable("ingest_report", err)
		}
		// 更早的字段时间不改当前值。相同时间仍接受后到的报文，历史照常保留。
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_property_current (tenant_id, device_id, identifier, value_json, reported_at_ms)
		VALUES (?, ?, ?, ?, ?) AS new_row
		ON DUPLICATE KEY UPDATE
		  value_json = IF(new_row.reported_at_ms >= device_property_current.reported_at_ms, new_row.value_json, device_property_current.value_json),
		  reported_at_ms = IF(new_row.reported_at_ms >= device_property_current.reported_at_ms, new_row.reported_at_ms, device_property_current.reported_at_ms)`,
			tenant, report.DeviceID, field.Identifier, raw, field.Time); err != nil {
			return false, s.unavailable("ingest_report", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO device_property_history
			(tenant_id, device_id, identifier, value_json, reported_at_ms, msg_id, payload_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			tenant, report.DeviceID, field.Identifier, raw, field.Time, report.MsgID, hash); err != nil {
			return false, s.unavailable("ingest_report", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, s.unavailable("ingest_report", err)
	}
	return false, nil
}

func fieldValue(field mqttmsg.Field) ([]byte, error) {
	if len(field.Value) == 0 {
		return nil, errors.New("empty field")
	}
	return field.Value, nil
}

// Properties 返回当前属性。没有上报过的功能点不出现。
func (s *Store) Properties(ctx context.Context, tenant, deviceID string) ([]Property, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identifier, value_json, reported_at_ms
		FROM device_property_current WHERE tenant_id = ? AND device_id = ? ORDER BY identifier`, tenant, deviceID)
	if err != nil {
		return nil, s.unavailable("properties", err)
	}
	defer rows.Close()
	var out []Property
	for rows.Next() {
		var item Property
		var raw []byte
		if err := rows.Scan(&item.Identifier, &raw, &item.ReportedAtMs); err != nil {
			return nil, s.unavailable("properties", err)
		}
		if err := json.Unmarshal(raw, &item.Value); err != nil {
			return nil, s.unavailable("properties", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("properties", err)
	}
	if out == nil {
		out = []Property{}
	}
	return out, nil
}

// HistoryCount 统计某个 msgId 留下的历史行数，供隔离测试核对去重。
func (s *Store) HistoryCount(ctx context.Context, tenant, deviceID, msgID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM device_property_history
		WHERE tenant_id = ? AND device_id = ? AND msg_id = ?`, tenant, deviceID, msgID).Scan(&n)
	if err != nil {
		return 0, s.unavailable("history_count", err)
	}
	return n, nil
}

// RejectInbound 记录无法解析的上行，不保存报文正文。
func (s *Store) RejectInbound(ctx context.Context, tenant, deviceID, topic, reason string) error {
	if !mqttmsg.ValidDeviceID(deviceID) {
		deviceID = "000000000000"
	}
	if len(reason) > 128 {
		reason = reason[:128]
	}
	if len(topic) > 255 {
		topic = topic[:255]
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_reject (tenant_id, device_id, topic, reason) VALUES (?, ?, ?, ?)`,
		tenant, deviceID, topic, reason)
	if err != nil {
		return s.unavailable("reject_inbound", err)
	}
	return nil
}

// SetReportedStatus 只更新设备自己发布的状态文本，不改 Broker 会话在线位。
func (s *Store) SetReportedStatus(ctx context.Context, tenant, deviceID, status string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE device_presence SET reported_status = ? WHERE tenant_id = ? AND device_id = ?`,
		status, tenant, deviceID)
	if err != nil {
		return s.unavailable("reported_status", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.unavailable("reported_status", err)
	}
	if n == 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO device_presence (tenant_id, device_id, reported_status) VALUES (?, ?, ?)`,
			tenant, deviceID, status)
		if err != nil && !isDuplicate(err) {
			return s.unavailable("reported_status", err)
		}
	}
	return nil
}

// ApplySession 按代数更新 MQTT 在线状态。rtc_connected 和 track_ready 保持原值。
func (s *Store) ApplySession(ctx context.Context, tenant, deviceID string, event presence.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.unavailable("presence", err)
	}
	defer tx.Rollback()
	current := presence.Snapshot{}
	var online, rtc, track int
	var reported string
	err = tx.QueryRowContext(ctx, `
		SELECT mqtt_online, generation_ms, connected_at_ms, disconnected_at_ms, rtc_connected, track_ready, reported_status
		FROM device_presence WHERE tenant_id = ? AND device_id = ? FOR UPDATE`, tenant, deviceID).Scan(
		&online, &current.Generation, &current.ConnectedAt, &current.DisconnectedAt, &rtc, &track, &reported)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return s.unavailable("presence", err)
	}
	current.Online = online == 1
	next, changed := presence.Apply(current, event)
	if !changed {
		return tx.Commit()
	}
	if err := upsertPresence(ctx, tx, tenant, deviceID, next, rtc, track, reported); err != nil {
		return s.unavailable("presence", err)
	}
	return tx.Commit()
}

// CalibratePresence 按 Broker 上仍连着的设备客户端校正 mqtt_online。
// 列表不完整时只补在线，避免把没扫到的设备误判成离线。rtc_connected、track_ready 和 reported_status 保持原值。
func (s *Store) CalibratePresence(ctx context.Context, tenant string, online map[string]int64, complete bool) error {
	if s == nil {
		return nil
	}
	targets := map[string]presenceTarget{}
	for id, at := range online {
		if mqttmsg.ValidDeviceID(id) {
			targets[id] = presenceTarget{connected: true, at: at}
		}
	}
	if complete {
		rows, err := s.db.QueryContext(ctx, `
			SELECT device_id, connected_at_ms FROM device_presence WHERE tenant_id = ? AND mqtt_online = 1`, tenant)
		if err != nil {
			return s.unavailable("presence", err)
		}
		for rows.Next() {
			var id string
			var connectedAt int64
			if err := rows.Scan(&id, &connectedAt); err != nil {
				rows.Close()
				return s.unavailable("presence", err)
			}
			if _, seen := targets[id]; !seen {
				targets[id] = presenceTarget{guard: connectedAt, guarded: true}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return s.unavailable("presence", err)
		}
	}
	for id, target := range targets {
		if err := s.calibrateDevice(ctx, tenant, id, target); err != nil {
			return err
		}
	}
	return nil
}

type presenceTarget struct {
	connected bool
	at        int64
	guard     int64
	guarded   bool
}

func (s *Store) calibrateDevice(ctx context.Context, tenant, deviceID string, target presenceTarget) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.unavailable("presence", err)
	}
	defer tx.Rollback()
	current := presence.Snapshot{}
	var online, rtc, track int
	var reported string
	err = tx.QueryRowContext(ctx, `
		SELECT mqtt_online, generation_ms, connected_at_ms, disconnected_at_ms, rtc_connected, track_ready, reported_status
		FROM device_presence WHERE tenant_id = ? AND device_id = ? FOR UPDATE`, tenant, deviceID).Scan(
		&online, &current.Generation, &current.ConnectedAt, &current.DisconnectedAt, &rtc, &track, &reported)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		if !target.connected {
			return tx.Commit()
		}
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM device WHERE tenant_id = ? AND device_id = ?`, tenant, deviceID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
	}
	if err != nil {
		return s.unavailable("presence", err)
	}
	current.Online = online == 1
	if target.guarded && current.ConnectedAt != target.guard {
		return tx.Commit()
	}
	next, changed := presence.Calibrate(current, target.connected, target.at)
	if !changed {
		return tx.Commit()
	}
	if err := upsertPresence(ctx, tx, tenant, deviceID, next, rtc, track, reported); err != nil {
		return s.unavailable("presence", err)
	}
	return tx.Commit()
}

func upsertPresence(ctx context.Context, tx *sql.Tx, tenant, deviceID string, next presence.Snapshot, rtc, track int, reported string) error {
	onlineValue := 0
	if next.Online {
		onlineValue = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO device_presence
		(tenant_id, device_id, mqtt_online, generation_ms, connected_at_ms, disconnected_at_ms, rtc_connected, track_ready, reported_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) AS new_row
		ON DUPLICATE KEY UPDATE
		  mqtt_online = new_row.mqtt_online,
		  generation_ms = new_row.generation_ms,
		  connected_at_ms = new_row.connected_at_ms,
		  disconnected_at_ms = new_row.disconnected_at_ms`,
		tenant, deviceID, onlineValue, next.Generation, next.ConnectedAt, next.DisconnectedAt, rtc, track, reported)
	return err
}

// GetStatus 返回分开的在线观察。没有会话行时 RTC 和轨道仍为 false。
func (s *Store) GetStatus(ctx context.Context, tenant, deviceID string) (Status, error) {
	status := Status{DeviceID: deviceID}
	var online, rtc, track int
	err := s.db.QueryRowContext(ctx, `
		SELECT mqtt_online, rtc_connected, track_ready, reported_status, generation_ms, connected_at_ms, disconnected_at_ms
		FROM device_presence WHERE tenant_id = ? AND device_id = ?`, tenant, deviceID).Scan(
		&online, &rtc, &track, &status.ReportedStatus, &status.GenerationMs, &status.ConnectedAtMs, &status.DisconnectedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return Status{}, s.unavailable("status", err)
	}
	status.MQTTOnline = online == 1
	status.RTCConnected = rtc == 1
	status.TrackReady = track == 1
	return status, nil
}
