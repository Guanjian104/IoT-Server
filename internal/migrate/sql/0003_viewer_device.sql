ALTER TABLE rtc_viewer ADD COLUMN device_id CHAR(12) NOT NULL DEFAULT '' AFTER session_id;

UPDATE rtc_viewer v JOIN rtc_session s ON s.tenant_id = v.tenant_id AND s.session_id = v.session_id SET v.device_id = s.device_id;

ALTER TABLE rtc_viewer DROP INDEX uk_viewer_idem;

ALTER TABLE rtc_viewer ADD UNIQUE KEY uk_viewer_idem (tenant_id, device_id, user_id, client_id, idempotency_key);
