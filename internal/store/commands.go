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
	Deadline time.Time
}

// commandDisposition 给出维护周期里一条命令的下一状态。空字符串表示这一轮不动。
// 规则与 Maintain 的语句顺序一致：先取消没有资格的排队命令，再重试仍有余量的发送，
// 截止时间一过就超时；uncertain 不自动重发，过期后交给人工。
func commandDisposition(state string, attempts int, deadlinePassed, stale, eligible bool) string {
	switch state {
	case "pending":
		if !eligible {
			return "cancelled"
		}
		if deadlinePassed {
			return "timeout"
		}
	case "sending":
		if stale && !eligible && !deadlinePassed {
			return "cancelled"
		}
		if deadlinePassed {
			return "timeout"
		}
		if stale && attempts < 3 {
			return "pending"
		}
		if stale && attempts >= 3 {
			return "needs_attention"
		}
	case "sent":
		if deadlinePassed {
			return "timeout"
		}
	case "uncertain":
		if deadlinePassed {
			return "needs_attention"
		}
	}
	return ""
}

// screenSend 在真正发布前再看一次截止时间和设备资格。
func screenSend(deadlinePassed, found, enabled, revoked bool) string {
	if deadlinePassed {
		return "timeout"
	}
	if !found || !enabled || revoked {
		return "cancelled"
	}
	return "send"
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

// PullPending 领取尚未交给 Broker 的命令。设备已禁用、吊销或消失时直接取消，不进入 sending。
func (s *Store) PullPending(ctx context.Context, tenant string, limit int) ([]Outbound, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, s.unavailable("pull_pending", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT command_id, device_id, payload, attempts, deadline_at
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
		if err := rows.Scan(&item.ID, &item.DeviceID, &item.Payload, &item.Attempts, &item.Deadline); err != nil {
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
	var outbound []Outbound
	for i := range picked {
		eligible, err := deviceEligible(ctx, tx, tenant, picked[i].DeviceID)
		if err != nil {
			return nil, s.unavailable("pull_pending", err)
		}
		if !eligible {
			if _, err := tx.ExecContext(ctx, `
				UPDATE device_command SET state = 'cancelled'
				WHERE command_id = ? AND state = 'pending'`, picked[i].ID); err != nil {
				return nil, s.unavailable("pull_pending", err)
			}
			continue
		}
		picked[i].Attempts++
		if _, err := tx.ExecContext(ctx, `
			UPDATE device_command SET state = 'sending', attempts = ? WHERE command_id = ? AND state = 'pending'`,
			picked[i].Attempts, picked[i].ID); err != nil {
			return nil, s.unavailable("pull_pending", err)
		}
		outbound = append(outbound, picked[i])
	}
	if err := tx.Commit(); err != nil {
		return nil, s.unavailable("pull_pending", err)
	}
	if outbound == nil {
		outbound = []Outbound{}
	}
	return outbound, nil
}

// PrepareSend 在发布前锁定仍处于 sending 的命令，复核截止时间和设备资格。
// 返回 send、timeout、cancelled 或 skip。决定发送时刷新更新时间，避免维护任务把在途命令取消。
func (s *Store) PrepareSend(ctx context.Context, commandID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", s.unavailable("prepare_send", err)
	}
	defer tx.Rollback()
	var tenant, deviceID, state string
	var deadline time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id, device_id, state, deadline_at
		FROM device_command WHERE command_id = ? FOR UPDATE`, commandID).Scan(&tenant, &deviceID, &state, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return "skip", tx.Commit()
	}
	if err != nil {
		return "", s.unavailable("prepare_send", err)
	}
	if state != "sending" {
		return "skip", tx.Commit()
	}
	found, enabled, revoked, err := deviceAccess(ctx, tx, tenant, deviceID)
	if err != nil {
		return "", s.unavailable("prepare_send", err)
	}
	action := screenSend(deadline.Before(time.Now().UTC()), found, enabled, revoked)
	if action == "send" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE device_command SET updated_at = UTC_TIMESTAMP(3)
			WHERE command_id = ? AND state = 'sending'`, commandID); err != nil {
			return "", s.unavailable("prepare_send", err)
		}
	} else if _, err := tx.ExecContext(ctx, `
		UPDATE device_command SET state = ?
		WHERE command_id = ? AND state = 'sending'`, action, commandID); err != nil {
		return "", s.unavailable("prepare_send", err)
	}
	if err := tx.Commit(); err != nil {
		return "", s.unavailable("prepare_send", err)
	}
	return action, nil
}

// ReleaseUnsent 把还没发布的 sending 放回 pending，并退回这次领取计入的次数。
func (s *Store) ReleaseUnsent(ctx context.Context, commandID string, attempts int) error {
	previous := attempts - 1
	if previous < 0 {
		previous = 0
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'pending', attempts = ?
		WHERE command_id = ? AND state = 'sending'`, previous, commandID)
	if err != nil {
		return s.unavailable("release_unsent", err)
	}
	return nil
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
// 明确失败且次数未用完回到 pending；次数用完进入 needs_attention。不确定是否发出时停在 uncertain，不自动重发。
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

// ConcludeSending 把仍在发送中的命令收成 timeout 或 needs_attention。其它状态不覆盖。
func (s *Store) ConcludeSending(ctx context.Context, commandID, state string) error {
	if state != "timeout" && state != "needs_attention" {
		return ErrConflict
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = ? WHERE command_id = ? AND state = 'sending'`, state, commandID)
	if err != nil {
		return s.unavailable("conclude_sending", err)
	}
	return nil
}

// ApplyReply 把设备应答应用到 sending、sent 或 uncertain。终态不回退。
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

// RetryCommand 把 uncertain 或 needs_attention 重新排队。设备没有资格时不重发。
// 次数清零，截止时间由调用方重新给出。同一条载荷会再次发布。
func (s *Store) RetryCommand(ctx context.Context, tenant, commandID string, deadline time.Time) (Command, error) {
	return s.settleCommand(ctx, tenant, commandID, deadline, true)
}

// CloseCommand 由人工把 uncertain 或 needs_attention 收成 failed，不再重发。
func (s *Store) CloseCommand(ctx context.Context, tenant, commandID string) (Command, error) {
	return s.settleCommand(ctx, tenant, commandID, time.Time{}, false)
}

func (s *Store) settleCommand(ctx context.Context, tenant, commandID string, deadline time.Time, retry bool) (Command, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Command{}, s.unavailable("settle_command", err)
	}
	defer tx.Rollback()
	var command Command
	var code sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT command_id, device_id, msg_id, state, reply_code, attempts
		FROM device_command WHERE tenant_id = ? AND command_id = ? FOR UPDATE`, tenant, commandID).Scan(
		&command.ID, &command.DeviceID, &command.MsgID, &command.State, &code, &command.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, s.unavailable("settle_command", err)
	}
	if code.Valid {
		value := int(code.Int64)
		command.ReplyCode = &value
	}
	if command.State != "uncertain" && command.State != "needs_attention" {
		return Command{}, ErrConflict
	}
	if retry {
		eligible, err := deviceEligible(ctx, tx, tenant, command.DeviceID)
		if err != nil {
			return Command{}, s.unavailable("settle_command", err)
		}
		if !eligible {
			return Command{}, ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE device_command
			SET state = 'pending', attempts = 0, reply_code = NULL, deadline_at = ?
			WHERE tenant_id = ? AND command_id = ? AND state = ?`,
			deadline.UTC(), tenant, commandID, command.State); err != nil {
			return Command{}, s.unavailable("settle_command", err)
		}
		command.State = "pending"
		command.Attempts = 0
		command.ReplyCode = nil
	} else if _, err := tx.ExecContext(ctx, `
		UPDATE device_command SET state = 'failed'
		WHERE tenant_id = ? AND command_id = ? AND state = ?`, tenant, commandID, command.State); err != nil {
		return Command{}, s.unavailable("settle_command", err)
	} else {
		command.State = "failed"
	}
	if err := tx.Commit(); err != nil {
		return Command{}, s.unavailable("settle_command", err)
	}
	return command, nil
}

// Maintain 让每一种未完成命令都有出口：取消、重试、超时，或交给人工。
func (s *Store) Maintain(ctx context.Context, tenant string) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE device_command AS c
		LEFT JOIN device AS d ON d.tenant_id = c.tenant_id AND d.device_id = c.device_id
		SET c.state = 'cancelled'
		WHERE c.tenant_id = ? AND c.state = 'pending'
		  AND (d.device_id IS NULL OR d.enabled <> 1 OR d.credential_revoked <> 0)`, tenant); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE device_command AS c
		LEFT JOIN device AS d ON d.tenant_id = c.tenant_id AND d.device_id = c.device_id
		SET c.state = 'cancelled'
		WHERE c.tenant_id = ? AND c.state = 'sending'
		  AND c.updated_at < (UTC_TIMESTAMP(3) - INTERVAL 15 SECOND)
		  AND c.deadline_at > UTC_TIMESTAMP(3)
		  AND (d.device_id IS NULL OR d.enabled <> 1 OR d.credential_revoked <> 0)`, tenant); err != nil {
		return 0, s.unavailable("maintain", err)
	}
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
		  AND attempts >= 3 AND deadline_at > UTC_TIMESTAMP(3)`, tenant); err != nil {
		return 0, s.unavailable("maintain", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'timeout'
		WHERE tenant_id = ? AND state IN ('pending', 'sending', 'sent') AND deadline_at < UTC_TIMESTAMP(3)`, tenant)
	if err != nil {
		return 0, s.unavailable("maintain", err)
	}
	timedOut, err := res.RowsAffected()
	if err != nil {
		return 0, s.unavailable("maintain", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE device_command SET state = 'needs_attention'
		WHERE tenant_id = ? AND state = 'uncertain' AND deadline_at < UTC_TIMESTAMP(3)`, tenant); err != nil {
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

func deviceEligible(ctx context.Context, tx *sql.Tx, tenant, deviceID string) (bool, error) {
	found, enabled, revoked, err := deviceAccess(ctx, tx, tenant, deviceID)
	if err != nil {
		return false, err
	}
	return found && enabled && !revoked, nil
}

func deviceAccess(ctx context.Context, tx *sql.Tx, tenant, deviceID string) (bool, bool, bool, error) {
	var enabled, revoked int
	err := tx.QueryRowContext(ctx, `
		SELECT enabled, credential_revoked FROM device
		WHERE tenant_id = ? AND device_id = ? FOR UPDATE`, tenant, deviceID).Scan(&enabled, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	return true, enabled == 1, revoked == 1, nil
}
