CREATE TABLE IF NOT EXISTS device_ticket (
  tenant_id VARCHAR(32) NOT NULL,
  ticket_hash CHAR(64) NOT NULL,
  device_id CHAR(12) NOT NULL,
  version INT NOT NULL,
  status VARCHAR(16) NOT NULL,
  expire_at TIMESTAMP(3) NULL,
  created_by BIGINT NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (ticket_hash),
  UNIQUE KEY uk_ticket_version (tenant_id, device_id, version),
  CONSTRAINT chk_ticket_status CHECK (status IN ('active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_ticket_current (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  ticket_hash CHAR(64) NOT NULL,
  version INT NOT NULL,
  PRIMARY KEY (tenant_id, device_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_session (
  tenant_id VARCHAR(32) NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  channel_id VARCHAR(32) NOT NULL,
  stream_type VARCHAR(32) NOT NULL,
  resolution VARCHAR(32) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL,
  generation BIGINT NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  device_ready TINYINT NOT NULL DEFAULT 0,
  media_ready TINYINT NOT NULL DEFAULT 0,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, session_id),
  KEY idx_session_device (tenant_id, device_id, status),
  CONSTRAINT chk_session_status CHECK (status IN ('CREATED', 'WAITING_DEVICE', 'STREAM_STARTING', 'ACTIVE', 'STOP_PENDING', 'STOPPING', 'CLOSED', 'FAILED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_stream_hold (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  channel_id VARCHAR(32) NOT NULL,
  stream_type VARCHAR(32) NOT NULL,
  resolution VARCHAR(32) NOT NULL DEFAULT '',
  session_id VARCHAR(32) NOT NULL,
  generation BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, device_id, channel_id, stream_type, resolution)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_stream_fence (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  channel_id VARCHAR(32) NOT NULL,
  stream_type VARCHAR(32) NOT NULL,
  resolution VARCHAR(32) NOT NULL DEFAULT '',
  request_id VARCHAR(32) NOT NULL,
  generation BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, device_id, channel_id, stream_type, resolution)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_viewer (
  tenant_id VARCHAR(32) NOT NULL,
  viewer_id VARCHAR(32) NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  user_id BIGINT NOT NULL,
  client_id VARCHAR(64) NOT NULL,
  idempotency_key VARCHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL,
  generation BIGINT NOT NULL,
  credential_token VARCHAR(128) NOT NULL,
  heartbeat_at TIMESTAMP(3) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, viewer_id),
  UNIQUE KEY uk_viewer_idem (tenant_id, user_id, client_id, idempotency_key),
  KEY idx_viewer_session (tenant_id, session_id, status),
  CONSTRAINT chk_viewer_status CHECK (status IN ('JOINING', 'ACTIVE', 'LEAVING', 'LEFT', 'DISCONNECTED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_call (
  tenant_id VARCHAR(32) NOT NULL,
  call_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  caller_user_id BIGINT NOT NULL,
  status VARCHAR(16) NOT NULL,
  generation BIGINT NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, call_id),
  KEY idx_call_device (tenant_id, device_id, status),
  CONSTRAINT chk_call_status CHECK (status IN ('CREATED', 'RINGING', 'ACCEPTED', 'CONNECTED', 'REJECTED', 'TIMEOUT', 'ENDED', 'FAILED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_call_hold (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  call_id VARCHAR(32) NOT NULL,
  PRIMARY KEY (tenant_id, device_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_task (
  tenant_id VARCHAR(32) NOT NULL,
  task_id VARCHAR(32) NOT NULL,
  task_type VARCHAR(32) NOT NULL,
  business_id VARCHAR(64) NOT NULL,
  generation BIGINT NOT NULL,
  status VARCHAR(16) NOT NULL,
  run_at TIMESTAMP(3) NOT NULL,
  attempts INT NOT NULL DEFAULT 0,
  lease_owner VARCHAR(64) NOT NULL DEFAULT '',
  lease_until TIMESTAMP(3) NULL,
  last_error VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, task_id),
  UNIQUE KEY uk_task_once (tenant_id, task_type, business_id, generation),
  KEY idx_task_due (status, run_at),
  CONSTRAINT chk_task_status CHECK (status IN ('pending', 'leased', 'done', 'cancelled', 'needs_attention'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_media_resource (
  tenant_id VARCHAR(32) NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  generation BIGINT NOT NULL,
  provider VARCHAR(32) NOT NULL,
  resource_type VARCHAR(32) NOT NULL,
  provider_resource_id VARCHAR(128) NOT NULL,
  PRIMARY KEY (tenant_id, session_id, resource_type),
  KEY idx_provider_resource (tenant_id, provider, resource_type, provider_resource_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_media_event (
  tenant_id VARCHAR(32) NOT NULL,
  event_id VARCHAR(64) NOT NULL,
  generation BIGINT NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  applied TINYINT NOT NULL,
  PRIMARY KEY (tenant_id, event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_control_plan (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  op VARCHAR(16) NOT NULL,
  result VARCHAR(16) NOT NULL,
  PRIMARY KEY (tenant_id, device_id, op),
  CONSTRAINT chk_plan_op CHECK (op IN ('start', 'stop', 'status')),
  CONSTRAINT chk_plan_result CHECK (result IN ('success', 'fail', 'unknown', 'timeout'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_participant (
  tenant_id VARCHAR(32) NOT NULL,
  session_id VARCHAR(32) NOT NULL,
  identity VARCHAR(64) NOT NULL,
  generation BIGINT NOT NULL,
  state VARCHAR(16) NOT NULL,
  PRIMARY KEY (tenant_id, session_id, identity)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rtc_audit (
  id BIGINT NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(32) NOT NULL,
  actor_user_id BIGINT NOT NULL,
  device_id CHAR(12) NOT NULL,
  action VARCHAR(32) NOT NULL,
  ref_id VARCHAR(64) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_audit_device (tenant_id, device_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
