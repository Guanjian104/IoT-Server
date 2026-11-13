package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"iot-server/internal/mqttmsg"
)

// Command 是一条已落库的下发。
type Command struct {
	ID        string
	DeviceID  string
	MsgID     string
	State     string
	ReplyCode *int
	Attempts  int
}

// Outbound 是工作器拿到锁之后可以发布的一条命令。
type Outbound struct {
	ID       string
	DeviceID string
	Payload  []byte
	Attempts int
}

// Enqueue 保存命令原文。发布发生在提交之后。
func (s *Store) Enqueue(ctx context.Context, tenant, deviceID, identifier string, value bool, deadline time.Time) (Command, error) {
	commandID, err := randomID("cmd-")
	if err != nil {
		return Command{}, s.unavailable("enqueue", err)
	}
	msgID, err := randomID("srv-")
	if err != nil {
		return Command{}, s.unavailable("enqueue", err)
	}
	payload, err := mqttmsg.CanonicalSet(msgID, time.Now().UTC().UnixMilli(), identifier, value)
	if err != nil {
		return Command{}, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO device_command (command_id, tenant_id, device_id, msg_id, state, payload, deadline_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?)`, commandID, tenant, deviceID, msgID, payload, deadline.UTC())
	if err != nil {
		return Command{}, s.unavailable("enqueue", err)
	}
	return Command{ID: commandID, DeviceID: deviceID, MsgID: msgID, State: "pending"}, nil
}

// GetCommand 按租户读取命令。
func (s *Store) GetCommand(ctx context.Context, tenant, commandID string) (Command, error) {
	var command Command
	var code sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT command_id, device_id, msg_id, state, reply_code, attempts
		FROM device_command WHERE tenant_id = ? AND command_id = ?`, tenant, commandID).Scan(
		&command.ID, &command.DeviceID, &command.MsgID, &command.State, &code, &command.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, s.unavailable("get_command", err)
	}
	if code.Valid {
		value := int(code.Int64)
		command.ReplyCode = &value
	}
	return command, nil
}

// PullPending 用 SKIP LOCKED 领取尚未交给 Broker 的命令，并标记为 sending。
func (s *Store) PullPending(ctx context.Context, tenant string, limit int) ([]Outbound, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, s.unavailable("pull_pending", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT command_id, device_id, payload, attempts
		FROM device_command
		WHERE tenant_id = ? AND state = 'pending' AND deadline_at > UTC_TIMESTAMP(3)
		ORDER BY created_at
		LIMIT ?
		FOR UPDATE SKIP LOCKED`, tenant, limit)
	if err != nil {
		return nil, s.unavailable("pull_pending", err)
	}
	var picked []Outbound
	for rows.Next() {
		var item Outbound
		if err := rows.Scan(&item.ID, &item.DeviceID, &item.Payload, &item.Attempts); err != nil {
			rows.Close()
			return nil, s.unavailable("pull_pending", err)
		}
		picked = append(picked, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, s.unavailable("pull_pending", err)
	}
	rows.Close()
	for i := range picked {
		picked[i].Attempts++
		if _, err := tx.ExecContext(ctx, `
			UPDATE device_command SET state = 'sending', attempts = ? WHERE command_id = ? AND state = 'pending'`,
			picked[i].Attempts, picked[i].ID); err != nil {
			return nil, s.unavailable("pull_pending", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, s.unavailable("pull_pending", err)
	}
	if picked == nil {
		picked = []Outbound{}
	}
	return picked, nil
}

// MarkSent 只在 Broker 接受发布后把 sending 改为 sent。已经进入终态时不覆盖。
func (s *Store) MarkSent(ctx context.Context, commandID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'sent' WHERE command_id = ? AND state = 'sending'`, commandID)
	if err != nil {
		return s.unavailable("mark_sent", err)
	}
	return nil
}

// MarkPublishResult 处理明确的发布失败，或无法确认 Broker 是否接受的情况。
func (s *Store) MarkPublishResult(ctx context.Context, commandID string, attempts int, uncertain bool) error {
	state := "pending"
	if uncertain {
		state = "uncertain"
	} else if attempts >= 3 {
		state = "needs_attention"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = ? WHERE command_id = ? AND state = 'sending'`, state, commandID)
	if err != nil {
		return s.unavailable("mark_publish", err)
	}
	return nil
}

// ApplyReply 把设备应答应用到仍在 sending 或 sent 的命令。终态不回退。
func (s *Store) ApplyReply(ctx context.Context, tenant, deviceID, msgID string, code int) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", s.unavailable("apply_reply", err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `
		SELECT state FROM device_command
		WHERE tenant_id = ? AND device_id = ? AND msg_id = ? FOR UPDATE`, tenant, deviceID, msgID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown", tx.Commit()
	}
	if err != nil {
		return "", s.unavailable("apply_reply", err)
	}
	next, ok := mqttmsg.ReplyState(state, code)
	if !ok {
		if state == "pending" {
			return "waiting", nil
		}
		return "late", tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE device_command SET state = ?, reply_code = ?
		WHERE tenant_id = ? AND device_id = ? AND msg_id = ? AND state = ?`,
		next, code, tenant, deviceID, msgID, state)
	if err != nil {
		return "", s.unavailable("apply_reply", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", s.unavailable("apply_reply", err)
	}
	if n == 0 {
		return "late", tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return "", s.unavailable("apply_reply", err)
	}
	return "applied", nil
}

// Maintain 回收卡住的 sending、把过期命令标成 timeout，并清理过期历史。
func (s *Store) Maintain(ctx context.Context, tenant string) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'pending'
		WHERE tenant_id = ? AND state = 'sending'
		  AND updated_at < (UTC_TIMESTAMP(3) - INTERVAL 15 SECOND)
		  AND attempts < 3 AND deadline_at > UTC_TIMESTAMP(3)`, tenant); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'needs_attention'
		WHERE tenant_id = ? AND state = 'sending'
		  AND updated_at < (UTC_TIMESTAMP(3) - INTERVAL 15 SECOND)
		  AND attempts >= 3`, tenant); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'timeout'
		WHERE tenant_id = ? AND state IN ('pending', 'sent') AND deadline_at < UTC_TIMESTAMP(3)`, tenant)
	if err != nil {
		return 0, s.unavailable("maintain", err)
	}
	timedOut, err := res.RowsAffected()
	if err != nil {
		return 0, s.unavailable("maintain", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM device_property_history
		WHERE created_at < (UTC_TIMESTAMP(3) - INTERVAL 7 DAY) LIMIT 500`); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM inbound_dedup
		WHERE created_at < (UTC_TIMESTAMP(3) - INTERVAL 7 DAY) LIMIT 500`); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	return int(timedOut), nil
}
