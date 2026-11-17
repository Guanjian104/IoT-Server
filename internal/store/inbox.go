package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

const inboxPoisonStop = 16

// InboxMessage 是已经落入数据库、还没处理完的入站报文。
type InboxMessage struct {
	Hash    string
	Topic   string
	Payload []byte
}

func inboxHash(topic string, payload []byte) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(topic))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write(payload)
	return hex.EncodeToString(sum.Sum(nil))
}

// StageInbox 把报文落成待处理。已经成功处理过的报文返回 done，调用方直接确认即可。
func (s *Store) StageInbox(ctx context.Context, tenant, topic string, payload []byte) (bool, error) {
	if s == nil || s.db == nil || topic == "" || len(topic) > 255 || len(payload) > 64*1024 {
		return false, ErrInvalid
	}
	hash := inboxHash(topic, payload)
	_, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO mqtt_inbox (tenant_id, msg_hash, topic, payload, status)
		VALUES (?, ?, ?, ?, 'pending')`, tenant, hash, topic, payload)
	if err != nil {
		return false, s.unavailable("inbox", err)
	}
	var status string
	err = s.db.QueryRowContext(ctx, `
		SELECT status FROM mqtt_inbox WHERE tenant_id = ? AND msg_hash = ?`, tenant, hash).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnavailable
	}
	if err != nil {
		return false, s.unavailable("inbox", err)
	}
	return status == "done", nil
}

// CompleteInbox 把报文标成已处理。重复完成没有副作用。
func (s *Store) CompleteInbox(ctx context.Context, tenant, topic string, payload []byte) error {
	if s == nil || s.db == nil {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE mqtt_inbox SET status = 'done', last_error = ''
		WHERE tenant_id = ? AND msg_hash = ? AND status IN ('pending', 'poison')`, tenant, inboxHash(topic, payload))
	if err != nil {
		return s.unavailable("inbox", err)
	}
	return nil
}

// PoisonInbox 把持续失败的报文从热路径挪开。正文留在库里，供后续对账。
func (s *Store) PoisonInbox(ctx context.Context, tenant, topic string, payload []byte, reason string) error {
	if s == nil || s.db == nil || topic == "" || len(topic) > 255 {
		return ErrInvalid
	}
	if len(reason) > 64 {
		reason = reason[:64]
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mqtt_inbox (tenant_id, msg_hash, topic, payload, attempts, status, last_error)
		VALUES (?, ?, ?, ?, 1, 'poison', ?) AS new_row
		ON DUPLICATE KEY UPDATE
		  status = IF(mqtt_inbox.status = 'done', mqtt_inbox.status, 'poison'),
		  attempts = IF(mqtt_inbox.status = 'done', mqtt_inbox.attempts, mqtt_inbox.attempts + 1),
		  last_error = IF(mqtt_inbox.status = 'done', mqtt_inbox.last_error, new_row.last_error)`,
		tenant, inboxHash(topic, payload), topic, payload, reason)
	if err != nil {
		return s.unavailable("inbox", err)
	}
	return nil
}

// PendingInbox 取出重启后还没处理完的报文。毒消息不在这里，避免再次堵住热路径。
func (s *Store) PendingInbox(ctx context.Context, tenant string, limit int) ([]InboxMessage, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT msg_hash, topic, payload FROM mqtt_inbox
		WHERE tenant_id = ? AND status = 'pending'
		ORDER BY created_at LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, s.unavailable("inbox", err)
	}
	defer rows.Close()
	var out []InboxMessage
	for rows.Next() {
		var item InboxMessage
		if err := rows.Scan(&item.Hash, &item.Topic, &item.Payload); err != nil {
			return nil, s.unavailable("inbox", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("inbox", err)
	}
	return out, nil
}

// PoisonInboxBatch 取出还可以再试一次的毒消息。
func (s *Store) PoisonInboxBatch(ctx context.Context, tenant string, limit int) ([]InboxMessage, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT msg_hash, topic, payload FROM mqtt_inbox
		WHERE tenant_id = ? AND status = 'poison' AND attempts < ?
		ORDER BY updated_at LIMIT ?`, tenant, inboxPoisonStop, limit)
	if err != nil {
		return nil, s.unavailable("inbox", err)
	}
	defer rows.Close()
	var out []InboxMessage
	for rows.Next() {
		var item InboxMessage
		if err := rows.Scan(&item.Hash, &item.Topic, &item.Payload); err != nil {
			return nil, s.unavailable("inbox", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("inbox", err)
	}
	return out, nil
}

// NotePoisonInbox 记录一次没有成功的对账。次数用尽后留在库里，不再自动重试。
func (s *Store) NotePoisonInbox(ctx context.Context, tenant, topic string, payload []byte, reason string) error {
	if len(reason) > 64 {
		reason = reason[:64]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE mqtt_inbox SET attempts = attempts + 1, last_error = ?
		WHERE tenant_id = ? AND msg_hash = ? AND status = 'poison'`, reason, tenant, inboxHash(topic, payload))
	if err != nil {
		return s.unavailable("inbox", err)
	}
	return nil
}

// SweepInbox 删掉已经完成一段时间的收件箱记录。
func (s *Store) SweepInbox(ctx context.Context, tenant string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM mqtt_inbox
		WHERE tenant_id = ? AND status = 'done' AND updated_at < DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 DAY)`, tenant)
	if err != nil {
		return s.unavailable("inbox", err)
	}
	return nil
}
