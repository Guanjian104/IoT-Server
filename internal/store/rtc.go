package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"iot-server/internal/media"
	"iot-server/internal/rtc"
	"iot-server/internal/streamctl"
)

const rtcAttempts = 8

var streamDim = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ConfigureRTC 设置测试用的延迟、宽限和响铃时间。零值保留内置默认。
func (s *Store) ConfigureRTC(stopDelay, joinGrace, ringTimeout time.Duration) {
	if s == nil {
		return
	}
	s.stopDelay = stopDelay
	s.joinGrace = joinGrace
	s.ringTimeout = ringTimeout
	s.taskLease = 15 * time.Second
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.unavailable("tx", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return s.unavailable("tx", err)
	}
	return nil
}

func (s *Store) delays() (time.Duration, time.Duration, time.Duration, time.Duration) {
	stop, grace, ring, lease := s.stopDelay, s.joinGrace, s.ringTimeout, s.taskLease
	if stop <= 0 {
		stop = 30 * time.Second
	}
	if grace <= 0 {
		grace = 15 * time.Second
	}
	if ring <= 0 {
		ring = 30 * time.Second
	}
	if lease <= 0 {
		lease = 15 * time.Second
	}
	return stop, grace, ring, lease
}

// TicketIssue 是只返回一次的票据明文。
type TicketIssue struct {
	Version int
	Expires time.Time
	Ticket  string
}

// WatchResult 是一次观看或幂等重试的结果。
type WatchResult struct {
	SessionID     string
	Generation    int64
	SessionStatus string
	ViewerID      string
	Credential    string
	Replayed      bool
}

// CallResult 是一次呼叫的结果。
type CallResult struct {
	CallID     string
	SessionID  string
	Generation int64
	Status     string
}

// SessionView 是会话查询结果。
type SessionView struct {
	SessionID   string
	DeviceID    string
	Status      string
	Generation  int64
	DeviceReady bool
	MediaReady  bool
	Occupants   int
}

// CallView 是通话查询结果。
type CallView struct {
	tenant     string
	CallID     string
	DeviceID   string
	SessionID  string
	Status     string
	Generation int64
	CallerID   int64
	CalleeID   int64
	AnsweredBy int64
}

type sessionRow struct {
	tenant      string
	SessionID   string
	DeviceID    string
	ChannelID   string
	StreamType  string
	Resolution  string
	Status      string
	Generation  int64
	Version     int64
	DeviceReady bool
	MediaReady  bool
}

type rtcTask struct {
	ID         string
	Type       string
	BusinessID string
	Generation int64
	Attempts   int
	Owner      string
	Token      int64
}

// errLeaseLost 表示这轮租约已经不属于当前执行者。旧结果必须整笔回滚。
var errLeaseLost = errors.New("lease")

func ownsLease(status, owner string, token int64, task rtcTask) bool {
	return status == "leased" && task.Owner != "" && owner == task.Owner && task.Token > 0 && token == task.Token
}

// IssueTicket 创建当前有效票据。库里只保存摘要。
func (s *Store) IssueTicket(ctx context.Context, user User, deviceID string) (TicketIssue, error) {
	return s.writeTicket(ctx, user, deviceID, false)
}

// RotateTicket 在同一事务里切换当前版本。旧版本不能再授权新的观看或通话。
func (s *Store) RotateTicket(ctx context.Context, user User, deviceID string) (TicketIssue, error) {
	return s.writeTicket(ctx, user, deviceID, true)
}

func (s *Store) writeTicket(ctx context.Context, user User, deviceID string, rotate bool) (TicketIssue, error) {
	plain, err := randomID("")
	if err != nil {
		return TicketIssue{}, s.unavailable("ticket", err)
	}
	plain += plain
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	expires := time.Now().UTC().Add(12 * time.Hour)
	var issued TicketIssue
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireOpenDevice(tx, ctx, user.TenantID, deviceID); err != nil {
			return err
		}
		if err := permit(ctx, tx, user, deviceID, "ticket"); err != nil {
			return err
		}
		version := 1
		if rotate {
			var current string
			err := tx.QueryRowContext(ctx, `
				SELECT ticket_hash, version FROM device_ticket_current
				WHERE tenant_id = ? AND device_id = ? FOR UPDATE`, user.TenantID, deviceID).Scan(&current, &version)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return s.unavailable("ticket", err)
			}
			version++
			if _, err = tx.ExecContext(ctx, `
				UPDATE device_ticket SET status = 'retired' WHERE ticket_hash = ?`, current); err != nil {
				return s.unavailable("ticket", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO device_ticket (tenant_id, ticket_hash, device_id, version, status, expire_at, created_by)
			VALUES (?, ?, ?, ?, 'active', ?, ?)`, user.TenantID, hash, deviceID, version, expires, user.ID); err != nil {
			return s.unavailable("ticket", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO device_ticket_current (tenant_id, device_id, ticket_hash, version) VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE ticket_hash = VALUES(ticket_hash), version = VALUES(version)`,
			user.TenantID, deviceID, hash, version); err != nil {
			return s.unavailable("ticket", err)
		}
		if err := audit(ctx, tx, user, deviceID, map[bool]string{true: "ticket_rotate", false: "ticket_create"}[rotate], hash[:12]); err != nil {
			return err
		}
		issued = TicketIssue{Version: version, Expires: expires, Ticket: plain}
		return nil
	})
	return issued, err
}

// Watch 校验票据和权限后加入当前会话，或在没有可复用会话时创建新轮次。
func (s *Store) Watch(ctx context.Context, user User, deviceID, ticket, clientID, idem, channel, stream, resolution string) (WatchResult, error) {
	if !validStream(channel, stream, resolution) || clientID == "" || len(clientID) > 64 || idem == "" || len(idem) > 64 {
		return WatchResult{}, ErrInvalid
	}
	var result WatchResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireOpenDevice(tx, ctx, user.TenantID, deviceID); err != nil {
			return err
		}
		if err := permit(ctx, tx, user, deviceID, "view"); err != nil {
			return err
		}
		if err := s.lockTicket(ctx, tx, user.TenantID, deviceID, ticket); err != nil {
			return err
		}
		if existing, ok, err := s.replayViewer(ctx, tx, user, deviceID, clientID, idem); err != nil || ok {
			result = existing
			return err
		}
		row, err := s.ensureSession(ctx, tx, user.TenantID, deviceID, channel, stream, resolution)
		if err != nil {
			return err
		}
		if row.Status == "STOP_PENDING" {
			row, err = s.resumeSession(ctx, tx, row)
			if err != nil {
				return err
			}
		}
		viewerID, err := randomID("v")
		if err != nil {
			return s.unavailable("viewer", err)
		}
		credential, err := randomID("c")
		if err != nil {
			return s.unavailable("viewer", err)
		}
		credential += credential
		_, err = tx.ExecContext(ctx, `
			INSERT INTO rtc_viewer
			(tenant_id, viewer_id, session_id, device_id, user_id, client_id, idempotency_key, status, generation, credential_token, heartbeat_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'JOINING', ?, ?, UTC_TIMESTAMP(3))`,
			user.TenantID, viewerID, row.SessionID, deviceID, user.ID, clientID, idem, row.Generation, credential)
		if dup(err) {
			replayed, ok, err := s.replayViewer(ctx, tx, user, deviceID, clientID, idem)
			result = replayed
			return errOr(ok, err)
		}
		if err != nil {
			return s.unavailable("viewer", err)
		}
		if err := audit(ctx, tx, user, deviceID, "watch", viewerID); err != nil {
			return err
		}
		result = WatchResult{SessionID: row.SessionID, Generation: row.Generation, SessionStatus: row.Status, ViewerID: viewerID, Credential: credential}
		return nil
	})
	return result, err
}

// HeartbeatViewer 只刷新仍处于 JOINING 或 ACTIVE 的当前轮次观看者。
func (s *Store) HeartbeatViewer(ctx context.Context, user User, viewerID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		row, session, err := s.lockViewer(ctx, tx, user.TenantID, viewerID)
		if err != nil {
			return err
		}
		if err := permit(ctx, tx, user, session.DeviceID, "view"); err != nil {
			return err
		}
		if row.userID != user.ID && !user.PlatformAdmin {
			return ErrForbidden
		}
		if _, err := rtc.NextViewer(row.status, "heartbeat"); err != nil {
			return ErrConflict
		}
		if !rtc.AcceptEvent(session.Generation, row.generation) {
			return ErrConflict
		}
		next := row.status
		if row.status == "JOINING" {
			next = "ACTIVE"
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE rtc_viewer SET status = ?, heartbeat_at = UTC_TIMESTAMP(3)
			WHERE tenant_id = ? AND viewer_id = ? AND status = ?`, next, user.TenantID, viewerID, row.status)
		if err != nil {
			return s.unavailable("viewer", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return nil
	})
}

// LeaveViewer 只让第一次退出改变占用。
func (s *Store) LeaveViewer(ctx context.Context, user User, viewerID string) error {
	_, grace, _, _ := s.delays()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		row, session, err := s.lockViewer(ctx, tx, user.TenantID, viewerID)
		if err != nil {
			return err
		}
		if err := permit(ctx, tx, user, session.DeviceID, "view"); err != nil {
			return err
		}
		if row.userID != user.ID && !user.PlatformAdmin {
			return ErrForbidden
		}
		if _, err := rtc.NextViewer(row.status, "leave"); err != nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_viewer SET status = 'LEFT' WHERE tenant_id = ? AND viewer_id = ? AND status = ?`,
			user.TenantID, viewerID, row.status); err != nil {
			return s.unavailable("viewer", err)
		}
		return s.afterOccupancy(ctx, tx, session, grace)
	})
}

// GetSession 返回会话和当前占用人数。
func (s *Store) GetSession(ctx context.Context, user User, sessionID string) (SessionView, error) {
	var view SessionView
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		row, err := loadSession(ctx, tx, user.TenantID, sessionID)
		if err != nil {
			return err
		}
		if err := permit(ctx, tx, user, row.DeviceID, "view"); err != nil {
			return err
		}
		_, grace, _, _ := s.delays()
		count, err := s.occupants(ctx, tx, user.TenantID, row.SessionID, grace)
		if err != nil {
			return err
		}
		view = sessionView(row, count)
		return nil
	})
	return view, err
}

// StartCall 在同一事务里占住设备主通话并创建会话。callee 是这路通话指定的接听人。
func (s *Store) StartCall(ctx context.Context, user User, deviceID, ticket, calleeName, channel, stream, resolution string) (CallResult, error) {
	if calleeName == "" || len(calleeName) > 64 || !validStream(channel, stream, resolution) {
		return CallResult{}, ErrInvalid
	}
	caps := (media.TestProvider{}).Capabilities()
	if !caps.SupportsAudio || !caps.SupportsVideo {
		return CallResult{}, ErrConflict
	}
	_, _, ring, _ := s.delays()
	var result CallResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireOpenDevice(tx, ctx, user.TenantID, deviceID); err != nil {
			return err
		}
		if err := permit(ctx, tx, user, deviceID, "call"); err != nil {
			return err
		}
		if err := s.lockTicket(ctx, tx, user.TenantID, deviceID, ticket); err != nil {
			return err
		}
		callee, err := s.userByName(ctx, user.TenantID, calleeName)
		if err != nil {
			return err
		}
		if callee.Status != "active" || callee.ID == user.ID {
			return ErrForbidden
		}
		if err := permit(ctx, tx, callee, deviceID, "call"); err != nil {
			return err
		}
		callID, err := randomID("c")
		if err != nil {
			return s.unavailable("call", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rtc_call_hold (tenant_id, device_id, call_id) VALUES (?, ?, ?)`, user.TenantID, deviceID, callID); dup(err) {
			return ErrBusy
		} else if err != nil {
			return s.unavailable("call", err)
		}
		row, err := s.ensureSession(ctx, tx, user.TenantID, deviceID, channel, stream, resolution)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rtc_call (tenant_id, call_id, device_id, session_id, caller_user_id, callee_user_id, status, generation)
			VALUES (?, ?, ?, ?, ?, ?, 'RINGING', ?)`, user.TenantID, callID, deviceID, row.SessionID, user.ID, callee.ID, row.Generation); err != nil {
			return s.unavailable("call", err)
		}
		if err := s.enqueue(ctx, tx, user.TenantID, "call_timeout", callID, row.Generation, time.Now().UTC().Add(ring)); err != nil {
			return err
		}
		if err := audit(ctx, tx, user, deviceID, "call_create", callID); err != nil {
			return err
		}
		result = CallResult{CallID: callID, SessionID: row.SessionID, Generation: row.Generation, Status: "RINGING"}
		return nil
	})
	return result, err
}

// AcceptCall 只允许这路通话指定的被叫接听。
func (s *Store) AcceptCall(ctx context.Context, user User, callID string) error {
	return s.moveCall(ctx, user, callID, "accept", false)
}

// RejectCall 只允许这路通话指定的被叫拒绝。
func (s *Store) RejectCall(ctx context.Context, user User, callID string) error {
	return s.moveCall(ctx, user, callID, "reject", false)
}

// EndCall 由主叫取消或挂断，振铃中的被叫可以拒绝，接通后的实际接听人也可以挂断。
func (s *Store) EndCall(ctx context.Context, user User, callID string) error {
	return s.moveCall(ctx, user, callID, "end", true)
}

// GetCall 返回通话当前状态。
func (s *Store) GetCall(ctx context.Context, user User, callID string) (CallView, error) {
	var view CallView
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		call, err := loadCall(ctx, tx, user.TenantID, callID)
		if err != nil {
			return err
		}
		if err := permit(ctx, tx, user, call.DeviceID, "view"); err != nil {
			if permit(ctx, tx, user, call.DeviceID, "call") != nil {
				return ErrForbidden
			}
		}
		view = call
		return nil
	})
	return view, err
}

// PutControlPlan 写入两个实例共享的测试脚本。空结果表示删除覆盖、恢复默认成功。
func (s *Store) PutControlPlan(ctx context.Context, tenant, deviceID, op, result string) error {
	if _, err := streamctl.Interpret(result); err != nil || (op != "start" && op != "stop" && op != "status") {
		return ErrInvalid
	}
	if result == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM rtc_control_plan WHERE tenant_id = ? AND device_id = ? AND op = ?`, tenant, deviceID, op)
		if err != nil {
			return s.unavailable("plan", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rtc_control_plan (tenant_id, device_id, op, result) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE result = VALUES(result)`, tenant, deviceID, op, result)
	if err != nil {
		return s.unavailable("plan", err)
	}
	return nil
}

// ApplyMediaEvent 按事件 ID 幂等处理。旧轮次只留痕，不改当前会话。
func (s *Store) ApplyMediaEvent(ctx context.Context, tenant, eventID, eventType, sessionID, identity string, generation int64) error {
	if eventID == "" || len(eventID) > 64 || sessionID == "" {
		return ErrInvalid
	}
	_, grace, _, _ := s.delays()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO rtc_media_event (tenant_id, event_id, generation, session_id, event_type, applied)
			VALUES (?, ?, ?, ?, ?, 0)`, tenant, eventID, generation, sessionID, eventType)
		if dup(err) {
			var applied int
			if err := tx.QueryRowContext(ctx, `SELECT applied FROM rtc_media_event WHERE tenant_id = ? AND event_id = ?`, tenant, eventID).Scan(&applied); err != nil {
				return s.unavailable("event", err)
			}
			if applied == 1 {
				return nil
			}
		} else if err != nil {
			return s.unavailable("event", err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			return s.unavailable("event", errors.New("event"))
		}
		row, err := peekSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if row.tenant != tenant {
			return ErrNotFound
		}
		if err := lockDevice(tx, ctx, tenant, row.DeviceID); err != nil {
			return err
		}
		row, err = loadSession(ctx, tx, tenant, sessionID)
		if err != nil {
			return err
		}
		current, holdOK, err := s.holdSession(ctx, tx, tenant, row.DeviceID, row.ChannelID, row.StreamType, row.Resolution)
		if err != nil {
			return err
		}
		if !holdOK || current != row.SessionID || !rtc.AcceptEvent(row.Generation, generation) {
			return markEvent(ctx, tx, tenant, eventID)
		}
		switch eventType {
		case "media.track_published":
			row.MediaReady = true
			row, err = s.saveSession(ctx, tx, row, "mark")
		case "media.track_unpublished":
			row.MediaReady = false
			row, err = s.saveSession(ctx, tx, row, "mark")
		case "media.participant_joined":
			err = s.upsertParticipant(ctx, tx, tenant, sessionID, identity, generation, "joined")
		case "media.participant_left":
			err = s.upsertParticipant(ctx, tx, tenant, sessionID, identity, generation, "left")
		case "media.room_closed":
			err = nil
		case "call.connected":
			err = s.connectCall(ctx, tx, tenant, sessionID, identity, generation)
		default:
			err = ErrInvalid
		}
		if err != nil {
			return err
		}
		if err := s.syncPresence(ctx, tx, tenant, row.DeviceID); err != nil {
			return err
		}
		if err := s.afterOccupancy(ctx, tx, row, grace); err != nil {
			return err
		}
		return markEvent(ctx, tx, tenant, eventID)
	})
}

// MaintainRTC 回收过期租约、领取到期任务，并从 MySQL 重建 Redis 索引。
func (s *Store) MaintainRTC(ctx context.Context, owner string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if len(owner) > 64 {
		owner = owner[:64]
	}
	if err := s.ensureExpire(ctx, owner); err != nil {
		return err
	}
	var tasks []rtcTask
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_task SET status = 'pending', lease_owner = ''
			WHERE status = 'leased' AND lease_until IS NOT NULL AND lease_until < UTC_TIMESTAMP(3)`); err != nil {
			return s.unavailable("task", err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT task_id, task_type, business_id, generation, attempts
			FROM rtc_task WHERE status = 'pending' AND run_at <= UTC_TIMESTAMP(3)
			ORDER BY run_at LIMIT 4 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return s.unavailable("task", err)
		}
		for rows.Next() {
			var task rtcTask
			if err := rows.Scan(&task.ID, &task.Type, &task.BusinessID, &task.Generation, &task.Attempts); err != nil {
				_ = rows.Close()
				return s.unavailable("task", err)
			}
			tasks = append(tasks, task)
		}
		if err := rows.Close(); err != nil {
			return s.unavailable("task", err)
		}
		if err := rows.Err(); err != nil {
			return s.unavailable("task", err)
		}
		_, _, _, lease := s.delays()
		until := time.Now().UTC().Add(lease)
		claimed := make([]rtcTask, 0, len(tasks))
		for i := range tasks {
			res, err := tx.ExecContext(ctx, `
				UPDATE rtc_task SET status = 'leased', attempts = attempts + 1, lease_owner = ?, lease_until = ?,
				lease_token = lease_token + 1
				WHERE task_id = ? AND status = 'pending'`, owner, until, tasks[i].ID)
			if err != nil {
				return s.unavailable("task", err)
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				continue
			}
			var token int64
			var held string
			if err := tx.QueryRowContext(ctx, `
				SELECT lease_owner, lease_token FROM rtc_task WHERE task_id = ?`, tasks[i].ID).Scan(&held, &token); err != nil {
				return s.unavailable("task", err)
			}
			tasks[i].Attempts++
			tasks[i].Owner = held
			tasks[i].Token = token
			claimed = append(claimed, tasks[i])
		}
		tasks = claimed
		return nil
	})
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := s.execTask(ctx, task); err != nil {
			_ = s.retryTask(ctx, task, err)
		}
	}
	s.rebuildIndex(ctx)
	return nil
}

func (s *Store) execTask(ctx context.Context, task rtcTask) error {
	switch task.Type {
	case "start_stream":
		return s.execStart(ctx, task)
	case "stop_stream":
		return s.execStop(ctx, task)
	case "call_timeout":
		return s.execTimeout(ctx, task)
	case "release_call":
		return s.execRelease(ctx, task)
	case "expire_viewers":
		return s.execExpire(ctx, task)
	default:
		return s.finishTask(ctx, task, "done", "")
	}
}

func (s *Store) execStart(ctx context.Context, task rtcTask) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.guardTask(ctx, tx, task); err != nil {
			return err
		}
		row, err := peekSession(ctx, tx, task.BusinessID)
		if errors.Is(err, ErrNotFound) {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		if err != nil {
			return err
		}
		if err := lockDevice(tx, ctx, row.tenant, row.DeviceID); err != nil {
			return err
		}
		row, err = loadSession(ctx, tx, row.tenant, task.BusinessID)
		if err != nil {
			return err
		}
		if row.Generation != task.Generation || (row.Status != "WAITING_DEVICE" && row.Status != "STREAM_STARTING") {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		open, err := deviceIsOpen(tx, ctx, row.tenant, row.DeviceID)
		if err != nil {
			return err
		}
		if !open {
			if err := s.failStartup(ctx, tx, row); err != nil {
				return err
			}
			return s.finishTaskTx(ctx, tx, task, "done", "disabled")
		}
		fenced, err := s.hasFence(ctx, tx, row)
		if err != nil {
			return err
		}
		if fenced {
			switch status, err := s.streamStatus(ctx, tx, row.tenant, row.DeviceID); {
			case err != nil:
				return err
			case status == "success":
				return s.openMedia(ctx, tx, row, task)
			case status == "fail":
				row, err = s.saveSession(ctx, tx, row, "fail")
				if err != nil {
					return err
				}
				if err := s.releaseHold(ctx, tx, row); err != nil {
					return err
				}
				return s.finishTaskTx(ctx, tx, task, "done", "fail")
			default:
				return s.holdUnknown(ctx, tx, task)
			}
		}
		outcome, err := s.plan(ctx, tx, row.tenant, row.DeviceID, "start")
		if err != nil {
			return err
		}
		switch outcome {
		case "success":
			return s.openMedia(ctx, tx, row, task)
		case "fail":
			row, err = s.saveSession(ctx, tx, row, "fail")
			if err != nil {
				return err
			}
			if err := s.releaseHold(ctx, tx, row); err != nil {
				return err
			}
			return s.finishTaskTx(ctx, tx, task, "done", "fail")
		case "unknown":
			if err := s.raiseFence(ctx, tx, row, task.ID); err != nil {
				return err
			}
			return s.holdUnknown(ctx, tx, task)
		default:
			return errors.New("timeout")
		}
	})
}

func (s *Store) execStop(ctx context.Context, task rtcTask) error {
	_, grace, _, _ := s.delays()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.guardTask(ctx, tx, task); err != nil {
			return err
		}
		row, err := peekSession(ctx, tx, task.BusinessID)
		if errors.Is(err, ErrNotFound) {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		if err != nil {
			return err
		}
		if err := lockDevice(tx, ctx, row.tenant, row.DeviceID); err != nil {
			return err
		}
		row, err = loadSession(ctx, tx, row.tenant, task.BusinessID)
		if err != nil {
			return err
		}
		if row.Generation != task.Generation {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		count, err := s.occupants(ctx, tx, row.tenant, row.SessionID, grace)
		if err != nil {
			return err
		}
		if count > 0 && (row.Status == "STOP_PENDING" || row.Status == "ACTIVE") {
			if row.Status == "STOP_PENDING" {
				if _, err = s.saveSession(ctx, tx, row, "resume"); err != nil {
					return err
				}
			}
			return s.finishTaskTx(ctx, tx, task, "cancelled", "occupant")
		}
		fenced, err := s.hasFence(ctx, tx, row)
		if err != nil {
			return err
		}
		if fenced {
			var raw sql.NullString
			err := tx.QueryRowContext(ctx, `
				SELECT result FROM rtc_control_plan WHERE tenant_id = ? AND device_id = ? AND op = 'status'`, row.tenant, row.DeviceID).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) || !raw.Valid || raw.String != "success" {
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return s.unavailable("plan", err)
				}
				return errors.New("unknown")
			}
			return s.closeHeld(ctx, tx, row, task)
		}
		if row.Status == "ACTIVE" {
			row, err = s.saveSession(ctx, tx, row, "empty")
			if err != nil {
				return err
			}
		}
		if row.Status == "STOP_PENDING" {
			row, err = s.saveSession(ctx, tx, row, "stop")
			if err != nil {
				return err
			}
		}
		if row.Status != "STOPPING" && row.Status != "CLOSED" {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		if row.Status == "CLOSED" {
			return s.finishTaskTx(ctx, tx, task, "done", "")
		}
		outcome, err := s.plan(ctx, tx, row.tenant, row.DeviceID, "stop")
		if err != nil {
			return err
		}
		switch outcome {
		case "success":
			return s.closeHeld(ctx, tx, row, task)
		case "unknown":
			if err := s.raiseFence(ctx, tx, row, task.ID); err != nil {
				return err
			}
			return s.holdUnknown(ctx, tx, task)
		case "fail":
			return errors.New("fail")
		default:
			return errors.New("timeout")
		}
	})
}

func (s *Store) execTimeout(ctx context.Context, task rtcTask) error {
	_, grace, _, _ := s.delays()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.guardTask(ctx, tx, task); err != nil {
			return err
		}
		call, err := peekCall(ctx, tx, task.BusinessID)
		if errors.Is(err, ErrNotFound) {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		if err != nil {
			return err
		}
		if err := lockDevice(tx, ctx, call.tenant, call.DeviceID); err != nil {
			return err
		}
		call, err = loadCall(ctx, tx, call.tenant, task.BusinessID)
		if err != nil {
			return err
		}
		if call.Generation != task.Generation || (call.Status != "RINGING" && call.Status != "ACCEPTED") {
			return s.finishTaskTx(ctx, tx, task, "done", "stale")
		}
		if call.Status == "ACCEPTED" {
			var updated time.Time
			if err := tx.QueryRowContext(ctx, `
				SELECT updated_at FROM rtc_call WHERE tenant_id = ? AND call_id = ?`, call.tenant, call.CallID).Scan(&updated); err != nil {
				return s.unavailable("call", err)
			}
			_, _, ring, _ := s.delays()
			if updated.Add(ring).After(time.Now().UTC()) {
				when := updated.Add(ring)
				return s.settleTask(ctx, tx, task, "pending", "", &when, true)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_call SET status = 'TIMEOUT' WHERE tenant_id = ? AND call_id = ? AND status = ? AND generation = ?`,
			call.tenant, call.CallID, call.Status, call.Generation); err != nil {
			return s.unavailable("call", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rtc_call_hold WHERE tenant_id = ? AND device_id = ? AND call_id = ?`, call.tenant, call.DeviceID, call.CallID); err != nil {
			return s.unavailable("call", err)
		}
		session, err := loadSession(ctx, tx, call.tenant, call.SessionID)
		if err != nil {
			return err
		}
		if err := s.afterOccupancy(ctx, tx, session, grace); err != nil {
			return err
		}
		return s.finishTaskTx(ctx, tx, task, "done", "")
	})
}

func (s *Store) execRelease(ctx context.Context, task rtcTask) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.guardTask(ctx, tx, task); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rtc_call_hold WHERE call_id = ?`, task.BusinessID); err != nil {
			return s.unavailable("call", err)
		}
		return s.finishTaskTx(ctx, tx, task, "done", "")
	})
}

func (s *Store) execExpire(ctx context.Context, task rtcTask) error {
	_, grace, ring, _ := s.delays()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.guardTask(ctx, tx, task); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_viewer SET status = 'DISCONNECTED'
			WHERE status IN ('JOINING', 'ACTIVE')
			  AND heartbeat_at <= DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ? MICROSECOND)`, grace.Microseconds()); err != nil {
			return s.unavailable("viewer", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_call SET status = 'TIMEOUT'
			WHERE status = 'ACCEPTED'
			  AND updated_at <= DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ? MICROSECOND)`, ring.Microseconds()); err != nil {
			return s.unavailable("call", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_call AS c
			JOIN rtc_session AS s ON s.tenant_id = c.tenant_id AND s.session_id = c.session_id
			SET c.status = 'ENDED'
			WHERE c.status = 'CONNECTED' AND s.media_ready = 0
			  AND c.updated_at <= DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ? MICROSECOND)`, grace.Microseconds()); err != nil {
			return s.unavailable("call", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_call AS c
			JOIN rtc_session AS sess ON sess.tenant_id = c.tenant_id AND sess.session_id = c.session_id
			SET c.status = 'ENDED'
			WHERE c.status = 'CONNECTED' AND sess.status IN ('FAILED', 'CLOSED')`); err != nil {
			return s.unavailable("call", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE h FROM rtc_call_hold AS h
			INNER JOIN rtc_call AS c ON c.tenant_id = h.tenant_id AND c.call_id = h.call_id
			WHERE c.status IN ('REJECTED', 'TIMEOUT', 'ENDED', 'FAILED')`); err != nil {
			return s.unavailable("call", err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT tenant_id, session_id, device_id FROM rtc_session
			WHERE status IN ('CREATED', 'WAITING_DEVICE', 'STREAM_STARTING', 'ACTIVE', 'STOP_PENDING')`)
		if err != nil {
			return s.unavailable("session", err)
		}
		defer rows.Close()
		type ref struct{ tenant, session, device string }
		var refs []ref
		for rows.Next() {
			var item ref
			if err := rows.Scan(&item.tenant, &item.session, &item.device); err != nil {
				return s.unavailable("session", err)
			}
			refs = append(refs, item)
		}
		if err := rows.Close(); err != nil {
			return s.unavailable("session", err)
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].tenant != refs[j].tenant {
				return refs[i].tenant < refs[j].tenant
			}
			return refs[i].device < refs[j].device
		})
		for _, item := range refs {
			if err := lockDevice(tx, ctx, item.tenant, item.device); err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			session, err := loadSession(ctx, tx, item.tenant, item.session)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			if session.Status == "CREATED" || session.Status == "WAITING_DEVICE" || session.Status == "STREAM_STARTING" {
				var updated time.Time
				if err := tx.QueryRowContext(ctx, `
					SELECT updated_at FROM rtc_session WHERE tenant_id = ? AND session_id = ?`, session.tenant, session.SessionID).Scan(&updated); err != nil {
					return s.unavailable("session", err)
				}
				if !updated.Add(ring).After(time.Now().UTC()) {
					if err := s.failStartup(ctx, tx, session); err != nil {
						return err
					}
				}
				continue
			}
			if session.Status != "ACTIVE" && session.Status != "STOP_PENDING" {
				continue
			}
			if err := s.afterOccupancy(ctx, tx, session, grace); err != nil {
				return err
			}
		}
		when := time.Now().UTC().Add(time.Second)
		return s.settleTask(ctx, tx, task, "pending", "", &when, true)
	})
	return err
}

func (s *Store) failStartup(ctx context.Context, tx *sql.Tx, row sessionRow) error {
	if row.Status != "CREATED" && row.Status != "WAITING_DEVICE" && row.Status != "STREAM_STARTING" {
		return nil
	}
	if _, err := s.saveSession(ctx, tx, row, "fail"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rtc_call SET status = 'FAILED'
		WHERE tenant_id = ? AND session_id = ? AND status IN ('CREATED', 'RINGING', 'ACCEPTED', 'CONNECTED')`,
		row.tenant, row.SessionID); err != nil {
		return s.unavailable("call", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE h FROM rtc_call_hold AS h
		INNER JOIN rtc_call AS c ON c.tenant_id = h.tenant_id AND c.call_id = h.call_id
		WHERE c.tenant_id = ? AND c.session_id = ? AND c.status = 'FAILED'`,
		row.tenant, row.SessionID); err != nil {
		return s.unavailable("call", err)
	}
	if err := s.releaseHold(ctx, tx, row); err != nil {
		return err
	}
	return s.syncPresence(ctx, tx, row.tenant, row.DeviceID)
}

func (s *Store) ensureSession(ctx context.Context, tx *sql.Tx, tenant, deviceID, channel, stream, resolution string) (sessionRow, error) {
	current, ok, err := s.holdSession(ctx, tx, tenant, deviceID, channel, stream, resolution)
	if err != nil {
		return sessionRow{}, err
	}
	if ok {
		row, err := loadSession(ctx, tx, tenant, current)
		if err != nil {
			return sessionRow{}, err
		}
		if row.Status == "STOPPING" || rtc.TerminalSession(row.Status) {
			return sessionRow{}, ErrStopping
		}
		return row, nil
	}
	if fenced, err := s.fenceKey(ctx, tx, tenant, deviceID, channel, stream, resolution); err != nil || fenced {
		if fenced {
			return sessionRow{}, ErrStopping
		}
		return sessionRow{}, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(generation), 0) FROM rtc_session
		WHERE tenant_id = ? AND device_id = ? AND channel_id = ? AND stream_type = ? AND resolution = ?`,
		tenant, deviceID, channel, stream, resolution).Scan(&generation); err != nil {
		return sessionRow{}, s.unavailable("session", err)
	}
	sessionID, err := randomID("s")
	if err != nil {
		return sessionRow{}, s.unavailable("session", err)
	}
	generation++
	row := sessionRow{SessionID: sessionID, DeviceID: deviceID, ChannelID: channel, StreamType: stream, Resolution: resolution, Status: "CREATED", Generation: generation, Version: 1, tenant: tenant}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_session
		(tenant_id, session_id, device_id, channel_id, stream_type, resolution, status, generation, version)
		VALUES (?, ?, ?, ?, ?, ?, 'CREATED', ?, 1)`, tenant, sessionID, deviceID, channel, stream, resolution, generation); err != nil {
		return sessionRow{}, s.unavailable("session", err)
	}
	row, err = s.saveSession(ctx, tx, row, "open")
	if err != nil {
		return sessionRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_stream_hold
		(tenant_id, device_id, channel_id, stream_type, resolution, session_id, generation)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, tenant, deviceID, channel, stream, resolution, row.SessionID, row.Generation); err != nil {
		return sessionRow{}, s.unavailable("session", err)
	}
	if err := s.enqueue(ctx, tx, tenant, "start_stream", row.SessionID, row.Generation, time.Now().UTC()); err != nil {
		return sessionRow{}, err
	}
	return row, nil
}

func (s *Store) resumeSession(ctx context.Context, tx *sql.Tx, row sessionRow) (sessionRow, error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE rtc_task SET status = 'cancelled' WHERE tenant_id = ? AND task_type = 'stop_stream' AND business_id = ? AND generation = ? AND status = 'pending'`,
		row.tenant, row.SessionID, row.Generation); err != nil {
		return sessionRow{}, s.unavailable("task", err)
	}
	return s.saveSession(ctx, tx, row, "resume")
}

func (s *Store) afterOccupancy(ctx context.Context, tx *sql.Tx, row sessionRow, grace time.Duration) error {
	if row.Status != "ACTIVE" && row.Status != "STOP_PENDING" {
		return nil
	}
	count, err := s.occupants(ctx, tx, row.tenant, row.SessionID, grace)
	if err != nil {
		return err
	}
	if count > 0 {
		if row.Status == "STOP_PENDING" {
			if _, err := s.resumeSession(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}
	if row.Status == "ACTIVE" {
		if _, err := s.saveSession(ctx, tx, row, "empty"); err != nil {
			return err
		}
	}
	stop, _, _, _ := s.delays()
	return s.enqueue(ctx, tx, row.tenant, "stop_stream", row.SessionID, row.Generation, time.Now().UTC().Add(stop))
}

func (s *Store) closeHeld(ctx context.Context, tx *sql.Tx, row sessionRow, task rtcTask) error {
	if row.Status == "STOPPING" {
		next, err := s.saveSession(ctx, tx, row, "closed")
		if err != nil {
			return err
		}
		row = next
	}
	if err := s.releaseHold(ctx, tx, row); err != nil {
		return err
	}
	if err := s.dropRoom(ctx, tx, row); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rtc_stream_fence WHERE tenant_id = ? AND device_id = ? AND channel_id = ? AND stream_type = ? AND resolution = ?`,
		row.tenant, row.DeviceID, row.ChannelID, row.StreamType, row.Resolution); err != nil {
		return s.unavailable("fence", err)
	}
	if err := s.syncPresence(ctx, tx, row.tenant, row.DeviceID); err != nil {
		return err
	}
	return s.finishTaskTx(ctx, tx, task, "done", "")
}

func (s *Store) saveSession(ctx context.Context, tx *sql.Tx, row sessionRow, event string) (sessionRow, error) {
	next, err := rtc.NextSession(row.Status, event, row.DeviceReady, row.MediaReady)
	if err != nil {
		if event != "mark" {
			return row, ErrConflict
		}
		next = row.Status
	}
	readyD, readyM := 0, 0
	if row.DeviceReady {
		readyD = 1
	}
	if row.MediaReady {
		readyM = 1
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE rtc_session SET status = ?, version = version + 1, device_ready = ?, media_ready = ?
		WHERE tenant_id = ? AND session_id = ? AND version = ? AND generation = ?`,
		next, readyD, readyM, row.tenant, row.SessionID, row.Version, row.Generation)
	if err != nil {
		return row, s.unavailable("session", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return row, ErrConflict
	}
	row.Status = next
	row.Version++
	return row, nil
}

func (s *Store) occupants(ctx context.Context, tx *sql.Tx, tenant, sessionID string, grace time.Duration) (int, error) {
	var viewers int
	cutoffMicros := grace.Microseconds()
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rtc_viewer WHERE tenant_id = ? AND session_id = ?
		AND status IN ('ACTIVE', 'JOINING')
		AND heartbeat_at > DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ? MICROSECOND)`,
		tenant, sessionID, cutoffMicros).Scan(&viewers); err != nil {
		return 0, s.unavailable("viewer", err)
	}
	var calls int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rtc_call WHERE tenant_id = ? AND session_id = ? AND status IN ('CREATED', 'RINGING', 'ACCEPTED', 'CONNECTED')`,
		tenant, sessionID).Scan(&calls); err != nil {
		return 0, s.unavailable("call", err)
	}
	return viewers + calls, nil
}

func (s *Store) moveCall(ctx context.Context, user User, callID, event string, end bool) error {
	_, grace, ring, _ := s.delays()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		call, err := peekCall(ctx, tx, callID)
		if err != nil {
			return err
		}
		if call.tenant != user.TenantID {
			return ErrNotFound
		}
		if err := lockDevice(tx, ctx, call.tenant, call.DeviceID); err != nil {
			return err
		}
		call, err = loadCall(ctx, tx, user.TenantID, callID)
		if err != nil {
			return err
		}
		if err := permit(ctx, tx, user, call.DeviceID, "call"); err != nil {
			return err
		}
		if !rtc.CanOperateCall(user.ID, call.CallerID, call.CalleeID, call.AnsweredBy, call.Status, event) {
			return ErrForbidden
		}
		action := event
		if end && call.Status == "RINGING" && user.ID == call.CallerID {
			action = "cancel"
		}
		if end && call.Status == "RINGING" && user.ID == call.CalleeID {
			action = "reject"
		}
		next, err := rtc.NextCall(call.Status, action)
		if err != nil {
			return ErrConflict
		}
		if action == "accept" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE rtc_call SET status = ?, answered_by = ?
				WHERE tenant_id = ? AND call_id = ? AND status = ? AND generation = ?`,
				next, user.ID, user.TenantID, callID, call.Status, call.Generation); err != nil {
				return s.unavailable("call", err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE rtc_call SET status = ? WHERE tenant_id = ? AND call_id = ? AND status = ? AND generation = ?`,
			next, user.TenantID, callID, call.Status, call.Generation); err != nil {
			return s.unavailable("call", err)
		}
		if next == "ACCEPTED" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE rtc_task SET run_at = ?
				WHERE tenant_id = ? AND task_type = 'call_timeout' AND business_id = ? AND generation = ? AND status = 'pending'`,
				time.Now().UTC().Add(ring), user.TenantID, callID, call.Generation); err != nil {
				return s.unavailable("task", err)
			}
		}
		if rtc.TerminalCall(next) {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM rtc_call_hold WHERE tenant_id = ? AND device_id = ? AND call_id = ?`, user.TenantID, call.DeviceID, callID); err != nil {
				return s.unavailable("call", err)
			}
			session, err := loadSession(ctx, tx, user.TenantID, call.SessionID)
			if err != nil {
				return err
			}
			if err := s.afterOccupancy(ctx, tx, session, grace); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) connectCall(ctx context.Context, tx *sql.Tx, tenant, sessionID, callID string, generation int64) error {
	if callID == "" {
		return ErrInvalid
	}
	call, err := loadCall(ctx, tx, tenant, callID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rtc.SameRound(call.SessionID, call.Generation, sessionID, generation) {
		return nil
	}
	next, err := rtc.NextCall(call.Status, "connect")
	if err != nil {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE rtc_call SET status = ? WHERE tenant_id = ? AND call_id = ? AND status = ? AND generation = ?`,
		next, tenant, callID, call.Status, call.Generation)
	if err != nil {
		return s.unavailable("call", err)
	}
	return nil
}

func (s *Store) lockTicket(ctx context.Context, tx *sql.Tx, tenant, deviceID, plain string) error {
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	var current string
	var expires sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT c.ticket_hash, t.expire_at
		FROM device_ticket_current c JOIN device_ticket t ON t.ticket_hash = c.ticket_hash
		WHERE c.tenant_id = ? AND c.device_id = ? AND t.status = 'active' FOR UPDATE`, tenant, deviceID).Scan(&current, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != hash) {
		return ErrTicket
	}
	if err != nil {
		return s.unavailable("ticket", err)
	}
	if expires.Valid && !expires.Time.After(time.Now().UTC()) {
		return ErrTicket
	}
	return nil
}

func (s *Store) replayViewer(ctx context.Context, tx *sql.Tx, user User, deviceID, clientID, idem string) (WatchResult, bool, error) {
	var result WatchResult
	var sessionID string
	err := tx.QueryRowContext(ctx, `
		SELECT viewer_id, session_id, credential_token FROM rtc_viewer
		WHERE tenant_id = ? AND device_id = ? AND user_id = ? AND client_id = ? AND idempotency_key = ?`,
		user.TenantID, deviceID, user.ID, clientID, idem).Scan(&result.ViewerID, &sessionID, &result.Credential)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchResult{}, false, nil
	}
	if err != nil {
		return WatchResult{}, false, s.unavailable("viewer", err)
	}
	row, err := loadSession(ctx, tx, user.TenantID, sessionID)
	if err != nil {
		return WatchResult{}, false, err
	}
	result.SessionID = row.SessionID
	result.Generation = row.Generation
	result.SessionStatus = row.Status
	result.Replayed = true
	return result, true, nil
}

func (s *Store) enqueue(ctx context.Context, tx *sql.Tx, tenant, kind, business string, generation int64, runAt time.Time) error {
	id, err := randomID("t")
	if err != nil {
		return s.unavailable("task", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO rtc_task (tenant_id, task_id, task_type, business_id, generation, status, run_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)`, tenant, id, kind, business, generation, runAt.UTC())
	if dup(err) {
		_, err = tx.ExecContext(ctx, `
			UPDATE rtc_task SET status = 'pending', run_at = ?, attempts = 0, lease_owner = '', lease_until = NULL, last_error = ''
			WHERE tenant_id = ? AND task_type = ? AND business_id = ? AND generation = ? AND status IN ('done', 'cancelled')`,
			runAt.UTC(), tenant, kind, business, generation)
	}
	if err != nil {
		return s.unavailable("task", err)
	}
	return nil
}

func (s *Store) holdUnknown(ctx context.Context, tx *sql.Tx, task rtcTask) error {
	if task.Attempts >= rtcAttempts {
		return s.finishTaskTx(ctx, tx, task, "needs_attention", "unknown")
	}
	when := time.Now().UTC().Add(time.Second)
	return s.settleTask(ctx, tx, task, "pending", "unknown", &when, false)
}

func (s *Store) streamStatus(ctx context.Context, tx *sql.Tx, tenant, deviceID string) (string, error) {
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT result FROM rtc_control_plan WHERE tenant_id = ? AND device_id = ? AND op = 'status'`, tenant, deviceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!raw.Valid || raw.String == "")) {
		return "unknown", nil
	}
	if err != nil {
		return "", s.unavailable("plan", err)
	}
	switch raw.String {
	case "success", "fail", "unknown", "timeout":
		return raw.String, nil
	default:
		return "unknown", nil
	}
}

// openMedia 创建或复用已经记下的房间。数据库没提交成功时删掉这次新建的房间。
func (s *Store) openMedia(ctx context.Context, tx *sql.Tx, row sessionRow, task rtcTask) error {
	var provider media.TestProvider
	var room media.Room
	existed := false
	err := tx.QueryRowContext(ctx, `
		SELECT provider_resource_id FROM rtc_media_resource
		WHERE tenant_id = ? AND session_id = ? AND resource_type = 'room'`, row.tenant, row.SessionID).Scan(&room.ID)
	switch {
	case err == nil && room.ID != "":
		existed = true
		room.SessionID = row.SessionID
	case errors.Is(err, sql.ErrNoRows):
		room, err = provider.CreateRoom(ctx, media.RoomRequest{SessionID: row.SessionID, Generation: row.Generation})
		if err != nil {
			return err
		}
	default:
		if err != nil {
			return s.unavailable("media", err)
		}
	}
	kept := existed
	defer func() {
		if !kept {
			_ = provider.DeleteRoom(ctx, room.ID)
		}
	}()
	if err := s.mapRoom(ctx, tx, row, room.ID); err != nil {
		return err
	}
	row.DeviceReady = true
	if row.Status == "WAITING_DEVICE" {
		row, err = s.saveSession(ctx, tx, row, "start")
		if err != nil {
			return err
		}
	}
	if _, err = s.saveSession(ctx, tx, row, "mark"); err != nil {
		return err
	}
	if err := s.syncPresence(ctx, tx, row.tenant, row.DeviceID); err != nil {
		return err
	}
	if err := s.finishTaskTx(ctx, tx, task, "done", ""); err != nil {
		return err
	}
	kept = true
	return nil
}

func (s *Store) plan(ctx context.Context, tx *sql.Tx, tenant, deviceID, op string) (string, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
		SELECT result FROM rtc_control_plan WHERE tenant_id = ? AND device_id = ? AND op = ?`, tenant, deviceID, op).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		raw = ""
	} else if err != nil {
		return "", s.unavailable("plan", err)
	}
	value, err := streamctl.Interpret(raw)
	if err != nil {
		return "", ErrInvalid
	}
	return value, nil
}

func (s *Store) mapRoom(ctx context.Context, tx *sql.Tx, row sessionRow, resourceID string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_media_resource
		(tenant_id, session_id, generation, provider, resource_type, provider_resource_id)
		VALUES (?, ?, ?, 'test', 'room', ?)`, row.tenant, row.SessionID, row.Generation, resourceID)
	if dup(err) {
		return nil
	}
	if err != nil {
		return s.unavailable("media", err)
	}
	return nil
}

func (s *Store) dropRoom(ctx context.Context, tx *sql.Tx, row sessionRow) error {
	var resourceID string
	err := tx.QueryRowContext(ctx, `
		SELECT provider_resource_id FROM rtc_media_resource
		WHERE tenant_id = ? AND session_id = ? AND resource_type = 'room'`, row.tenant, row.SessionID).Scan(&resourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return s.unavailable("media", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rtc_media_resource WHERE tenant_id = ? AND session_id = ? AND resource_type = 'room'`, row.tenant, row.SessionID); err != nil {
		return s.unavailable("media", err)
	}
	var left int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rtc_media_resource WHERE tenant_id = ? AND provider = 'test' AND resource_type = 'room' AND provider_resource_id = ?`,
		row.tenant, resourceID).Scan(&left); err != nil {
		return s.unavailable("media", err)
	}
	if left == 0 {
		_ = media.TestProvider{}.DeleteRoom(ctx, resourceID)
	}
	return nil
}

func (s *Store) raiseFence(ctx context.Context, tx *sql.Tx, row sessionRow, requestID string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_stream_fence
		(tenant_id, device_id, channel_id, stream_type, resolution, request_id, generation)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE request_id = VALUES(request_id), generation = VALUES(generation)`,
		row.tenant, row.DeviceID, row.ChannelID, row.StreamType, row.Resolution, requestID, row.Generation)
	if err != nil {
		return s.unavailable("fence", err)
	}
	return nil
}

func (s *Store) hasFence(ctx context.Context, tx *sql.Tx, row sessionRow) (bool, error) {
	return s.fenceKey(ctx, tx, row.tenant, row.DeviceID, row.ChannelID, row.StreamType, row.Resolution)
}

func (s *Store) fenceKey(ctx context.Context, tx *sql.Tx, tenant, deviceID, channel, stream, resolution string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM rtc_stream_fence WHERE tenant_id = ? AND device_id = ? AND channel_id = ? AND stream_type = ? AND resolution = ?`,
		tenant, deviceID, channel, stream, resolution).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, s.unavailable("fence", err)
	}
	return true, nil
}

func (s *Store) releaseHold(ctx context.Context, tx *sql.Tx, row sessionRow) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM rtc_stream_hold WHERE tenant_id = ? AND device_id = ? AND channel_id = ? AND stream_type = ? AND resolution = ? AND session_id = ? AND generation = ?`,
		row.tenant, row.DeviceID, row.ChannelID, row.StreamType, row.Resolution, row.SessionID, row.Generation)
	if err != nil {
		return s.unavailable("session", err)
	}
	return nil
}

func (s *Store) holdSession(ctx context.Context, tx *sql.Tx, tenant, deviceID, channel, stream, resolution string) (string, bool, error) {
	var sessionID string
	err := tx.QueryRowContext(ctx, `
		SELECT session_id FROM rtc_stream_hold
		WHERE tenant_id = ? AND device_id = ? AND channel_id = ? AND stream_type = ? AND resolution = ? FOR UPDATE`,
		tenant, deviceID, channel, stream, resolution).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, s.unavailable("session", err)
	}
	return sessionID, true, nil
}

func (s *Store) syncPresence(ctx context.Context, tx *sql.Tx, tenant, deviceID string) error {
	var tracks int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rtc_session s
		JOIN rtc_stream_hold h ON h.tenant_id = s.tenant_id AND h.session_id = s.session_id AND h.generation = s.generation
		WHERE s.tenant_id = ? AND s.device_id = ? AND s.status = 'ACTIVE' AND s.media_ready = 1`, tenant, deviceID).Scan(&tracks); err != nil {
		return s.unavailable("presence", err)
	}
	var connected int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rtc_participant p
		JOIN rtc_stream_hold h ON h.tenant_id = p.tenant_id AND h.session_id = p.session_id AND h.generation = p.generation
		WHERE p.tenant_id = ? AND h.device_id = ? AND p.identity = ? AND p.state = 'joined'`,
		tenant, deviceID, "device:"+deviceID).Scan(&connected); err != nil {
		return s.unavailable("presence", err)
	}
	track, rtcOn := 0, 0
	if tracks > 0 {
		track = 1
	}
	if connected > 0 {
		rtcOn = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE device_presence SET track_ready = ?, rtc_connected = ? WHERE tenant_id = ? AND device_id = ?`, track, rtcOn, tenant, deviceID)
	if err != nil {
		return s.unavailable("presence", err)
	}
	return nil
}

func (s *Store) upsertParticipant(ctx context.Context, tx *sql.Tx, tenant, sessionID, identity string, generation int64, state string) error {
	if identity == "" || len(identity) > 64 {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_participant (tenant_id, session_id, identity, generation, state) VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE generation = VALUES(generation), state = VALUES(state)`,
		tenant, sessionID, identity, generation, state)
	if err != nil {
		return s.unavailable("participant", err)
	}
	return nil
}

func (s *Store) ensureExpire(ctx context.Context, tenantHint string) error {
	_ = tenantHint
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rtc_task (tenant_id, task_id, task_type, business_id, generation, status, run_at)
		SELECT 'dev', 'texpireviewers0001', 'expire_viewers', 'viewers', 0, 'pending', UTC_TIMESTAMP(3)
		FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM rtc_task WHERE task_type = 'expire_viewers' AND business_id = 'viewers')`)
	if err != nil {
		return s.unavailable("task", err)
	}
	return nil
}

func (s *Store) finishTask(ctx context.Context, task rtcTask, status, reason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.finishTaskTx(ctx, tx, task, status, reason)
	})
}

func (s *Store) finishTaskTx(ctx context.Context, tx *sql.Tx, task rtcTask, status, reason string) error {
	return s.settleTask(ctx, tx, task, status, reason, nil, false)
}

func (s *Store) settleTask(ctx context.Context, tx *sql.Tx, task rtcTask, status, reason string, runAt *time.Time, reset bool) error {
	resetBit := 0
	if reset {
		resetBit = 1
	}
	var run any
	if runAt != nil {
		run = runAt.UTC()
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE rtc_task
		SET status = ?, last_error = ?, lease_owner = '', lease_until = NULL,
		    run_at = COALESCE(?, run_at), attempts = IF(? = 1, 0, attempts)
		WHERE task_id = ? AND status = 'leased' AND lease_owner = ? AND lease_token = ?`,
		status, reason, run, resetBit, task.ID, task.Owner, task.Token)
	if err != nil {
		return s.unavailable("task", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errLeaseLost
	}
	return nil
}

func (s *Store) guardTask(ctx context.Context, tx *sql.Tx, task rtcTask) error {
	var status, owner string
	var token int64
	err := tx.QueryRowContext(ctx, `
		SELECT status, lease_owner, lease_token FROM rtc_task WHERE task_id = ?`, task.ID).Scan(&status, &owner, &token)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !ownsLease(status, owner, token, task)) {
		return errLeaseLost
	}
	if err != nil {
		return s.unavailable("task", err)
	}
	return nil
}

func (s *Store) retryTask(ctx context.Context, task rtcTask, cause error) error {
	if errors.Is(cause, errLeaseLost) {
		return nil
	}
	status := "pending"
	if task.Attempts >= rtcAttempts {
		status = "needs_attention"
	}
	reason := "timeout"
	if cause != nil && cause.Error() == "unknown" {
		reason = "unknown"
	} else if cause != nil && cause.Error() == "fail" {
		reason = "fail"
	}
	when := time.Now().UTC().Add(time.Second)
	res, err := s.db.ExecContext(ctx, `
		UPDATE rtc_task SET status = ?, last_error = ?, lease_owner = '', lease_until = NULL, run_at = ?
		WHERE task_id = ? AND status = 'leased' AND lease_owner = ? AND lease_token = ?`,
		status, reason, when, task.ID, task.Owner, task.Token)
	if err != nil {
		return s.unavailable("task", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil
	}
	return nil
}

func (s *Store) rebuildIndex(ctx context.Context) {
	if s.rdb == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, UNIX_TIMESTAMP(run_at) FROM rtc_task WHERE status = 'pending' LIMIT 100`)
	if err != nil {
		return
	}
	defer rows.Close()
	var members []redis.Z
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return
		}
		members = append(members, redis.Z{Score: score, Member: id})
	}
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, "rtc:task-index")
	if len(members) > 0 {
		pipe.ZAdd(ctx, "rtc:task-index", members...)
	}
	_, _ = pipe.Exec(ctx)
}

func loadSession(ctx context.Context, tx *sql.Tx, tenant, sessionID string) (sessionRow, error) {
	return scanSession(tx.QueryRowContext(ctx, `
		SELECT tenant_id, session_id, device_id, channel_id, stream_type, resolution, status, generation, version, device_ready, media_ready
		FROM rtc_session WHERE tenant_id = ? AND session_id = ? FOR UPDATE`, tenant, sessionID))
}

func loadSessionAny(ctx context.Context, tx *sql.Tx, sessionID string) (sessionRow, error) {
	return scanSession(tx.QueryRowContext(ctx, `
		SELECT tenant_id, session_id, device_id, channel_id, stream_type, resolution, status, generation, version, device_ready, media_ready
		FROM rtc_session WHERE session_id = ? FOR UPDATE`, sessionID))
}

func peekSession(ctx context.Context, tx *sql.Tx, sessionID string) (sessionRow, error) {
	return scanSession(tx.QueryRowContext(ctx, `
		SELECT tenant_id, session_id, device_id, channel_id, stream_type, resolution, status, generation, version, device_ready, media_ready
		FROM rtc_session WHERE session_id = ?`, sessionID))
}

func scanSession(row *sql.Row) (sessionRow, error) {
	var out sessionRow
	var deviceReady, mediaReady int
	err := row.Scan(&out.tenant, &out.SessionID, &out.DeviceID, &out.ChannelID, &out.StreamType, &out.Resolution, &out.Status, &out.Generation, &out.Version, &deviceReady, &mediaReady)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRow{}, ErrNotFound
	}
	if err != nil {
		return sessionRow{}, ErrUnavailable
	}
	out.DeviceReady = deviceReady == 1
	out.MediaReady = mediaReady == 1
	return out, nil
}

type viewerRow struct {
	userID     int64
	status     string
	generation int64
}

func (s *Store) lockViewer(ctx context.Context, tx *sql.Tx, tenant, viewerID string) (viewerRow, sessionRow, error) {
	var sessionID, deviceID string
	err := tx.QueryRowContext(ctx, `
		SELECT v.session_id, s.device_id FROM rtc_viewer v
		JOIN rtc_session s ON s.tenant_id = v.tenant_id AND s.session_id = v.session_id
		WHERE v.tenant_id = ? AND v.viewer_id = ?`, tenant, viewerID).Scan(&sessionID, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return viewerRow{}, sessionRow{}, ErrNotFound
	}
	if err != nil {
		return viewerRow{}, sessionRow{}, s.unavailable("viewer", err)
	}
	if err := lockDevice(tx, ctx, tenant, deviceID); err != nil {
		return viewerRow{}, sessionRow{}, err
	}
	var row viewerRow
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, status, generation FROM rtc_viewer WHERE tenant_id = ? AND viewer_id = ? FOR UPDATE`,
		tenant, viewerID).Scan(&row.userID, &row.status, &row.generation)
	if errors.Is(err, sql.ErrNoRows) {
		return viewerRow{}, sessionRow{}, ErrNotFound
	}
	if err != nil {
		return viewerRow{}, sessionRow{}, s.unavailable("viewer", err)
	}
	session, err := loadSession(ctx, tx, tenant, sessionID)
	if err != nil {
		return viewerRow{}, sessionRow{}, err
	}
	return row, session, nil
}

func loadCall(ctx context.Context, tx *sql.Tx, tenant, callID string) (CallView, error) {
	view, err := scanCall(tx.QueryRowContext(ctx, `
		SELECT tenant_id, call_id, device_id, session_id, caller_user_id, COALESCE(callee_user_id, 0), COALESCE(answered_by, 0), status, generation
		FROM rtc_call WHERE tenant_id = ? AND call_id = ? FOR UPDATE`, tenant, callID))
	return view, err
}

func peekCall(ctx context.Context, tx *sql.Tx, callID string) (CallView, error) {
	return scanCall(tx.QueryRowContext(ctx, `
		SELECT tenant_id, call_id, device_id, session_id, caller_user_id, COALESCE(callee_user_id, 0), COALESCE(answered_by, 0), status, generation
		FROM rtc_call WHERE call_id = ?`, callID))
}

func loadCallAny(ctx context.Context, tx *sql.Tx, callID string) (CallView, error) {
	return scanCall(tx.QueryRowContext(ctx, `
		SELECT tenant_id, call_id, device_id, session_id, caller_user_id, COALESCE(callee_user_id, 0), COALESCE(answered_by, 0), status, generation
		FROM rtc_call WHERE call_id = ? FOR UPDATE`, callID))
}

func scanCall(row *sql.Row) (CallView, error) {
	var view CallView
	err := row.Scan(&view.tenant, &view.CallID, &view.DeviceID, &view.SessionID, &view.CallerID, &view.CalleeID, &view.AnsweredBy, &view.Status, &view.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return CallView{}, ErrNotFound
	}
	if err != nil {
		return CallView{}, ErrUnavailable
	}
	return view, nil
}

// lockDevice 锁住设备行，给退出、挂断和回收用。设备已禁用或吊销时仍然可以清理旧业务。
func lockDevice(tx *sql.Tx, ctx context.Context, tenant, deviceID string) error {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM device WHERE tenant_id = ? AND device_id = ? FOR UPDATE`, tenant, deviceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

// requireOpenDevice 锁住设备，并且只放行仍可接入的设备。新的票据、观看和通话走这里。
func requireOpenDevice(tx *sql.Tx, ctx context.Context, tenant, deviceID string) error {
	if err := lockDevice(tx, ctx, tenant, deviceID); err != nil {
		return err
	}
	open, err := deviceIsOpen(tx, ctx, tenant, deviceID)
	if err != nil {
		return err
	}
	if !open {
		return ErrConflict
	}
	return nil
}

func deviceIsOpen(tx *sql.Tx, ctx context.Context, tenant, deviceID string) (bool, error) {
	var enabled, revoked int
	err := tx.QueryRowContext(ctx, `
		SELECT enabled, credential_revoked FROM device WHERE tenant_id = ? AND device_id = ?`, tenant, deviceID).Scan(&enabled, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, ErrUnavailable
	}
	return enabled == 1 && revoked == 0, nil
}

func permit(ctx context.Context, tx *sql.Tx, user User, deviceID, perm string) error {
	if user.PlatformAdmin {
		return nil
	}
	query := `SELECT COUNT(*) FROM user_device_grant WHERE tenant_id = ? AND user_id = ? AND device_id = ? AND permission = ?`
	args := []any{user.TenantID, user.ID, deviceID, perm}
	if perm == "view" {
		query = `SELECT COUNT(*) FROM user_device_grant WHERE tenant_id = ? AND user_id = ? AND device_id = ? AND permission IN ('view', 'manage')`
		args = args[:3]
	}
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return ErrUnavailable
	}
	if n == 0 {
		return ErrForbidden
	}
	return nil
}

func audit(ctx context.Context, tx *sql.Tx, user User, deviceID, action, ref string) error {
	if len(ref) > 64 {
		ref = ref[:64]
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rtc_audit (tenant_id, actor_user_id, device_id, action, ref_id) VALUES (?, ?, ?, ?, ?)`,
		user.TenantID, user.ID, deviceID, action, ref)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func markEvent(ctx context.Context, tx *sql.Tx, tenant, eventID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE rtc_media_event SET applied = 1 WHERE tenant_id = ? AND event_id = ?`, tenant, eventID)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func sessionView(row sessionRow, count int) SessionView {
	return SessionView{SessionID: row.SessionID, DeviceID: row.DeviceID, Status: row.Status, Generation: row.Generation, DeviceReady: row.DeviceReady, MediaReady: row.MediaReady, Occupants: count}
}

func validStream(channel, stream, resolution string) bool {
	if !streamDim.MatchString(channel) || !streamDim.MatchString(stream) {
		return false
	}
	return resolution == "" || streamDim.MatchString(resolution)
}

func dup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func errOr(ok bool, err error) error {
	if err != nil {
		return err
	}
	if !ok {
		return ErrConflict
	}
	return nil
}
