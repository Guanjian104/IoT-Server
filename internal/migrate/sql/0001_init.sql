CREATE TABLE IF NOT EXISTS schema_migrations (
  version VARCHAR(64) NOT NULL,
  applied_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS app_user (
  id BIGINT NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(32) NOT NULL,
  username VARCHAR(64) NOT NULL,
  password_hash VARCHAR(255) NOT NULL,
  platform_admin TINYINT NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_user (tenant_id, username),
  CONSTRAINT chk_user_status CHECK (status IN ('active', 'disabled')),
  CONSTRAINT chk_user_admin CHECK (platform_admin IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS api_token (
  token_hash CHAR(64) NOT NULL,
  user_id BIGINT NOT NULL,
  expires_at TIMESTAMP(3) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (token_hash),
  KEY idx_token_user (user_id, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS product (
  tenant_id VARCHAR(32) NOT NULL,
  product_id VARCHAR(64) NOT NULL,
  name VARCHAR(128) NOT NULL,
  PRIMARY KEY (tenant_id, product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  product_id VARCHAR(64) NOT NULL,
  enabled TINYINT NOT NULL DEFAULT 1,
  credential_revoked TINYINT NOT NULL DEFAULT 0,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, device_id),
  CONSTRAINT chk_device_enabled CHECK (enabled IN (0, 1)),
  CONSTRAINT chk_device_revoked CHECK (credential_revoked IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_capability (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  identifier VARCHAR(64) NOT NULL,
  display_name VARCHAR(64) NOT NULL,
  data_type VARCHAR(16) NOT NULL,
  writable TINYINT NOT NULL,
  PRIMARY KEY (tenant_id, device_id, identifier),
  CONSTRAINT chk_capability_writable CHECK (writable IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS user_device_grant (
  tenant_id VARCHAR(32) NOT NULL,
  user_id BIGINT NOT NULL,
  device_id CHAR(12) NOT NULL,
  permission VARCHAR(16) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, user_id, device_id, permission),
  CONSTRAINT chk_grant_permission CHECK (permission IN ('manage', 'view', 'call', 'ticket'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_presence (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  mqtt_online TINYINT NOT NULL DEFAULT 0,
  generation_ms BIGINT NOT NULL DEFAULT 0,
  connected_at_ms BIGINT NOT NULL DEFAULT 0,
  disconnected_at_ms BIGINT NOT NULL DEFAULT 0,
  rtc_connected TINYINT NOT NULL DEFAULT 0,
  track_ready TINYINT NOT NULL DEFAULT 0,
  reported_status VARCHAR(16) NOT NULL DEFAULT '',
  updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, device_id),
  CONSTRAINT chk_presence_flags CHECK (mqtt_online IN (0, 1) AND rtc_connected IN (0, 1) AND track_ready IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_property_current (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  identifier VARCHAR(64) NOT NULL,
  value_json JSON NOT NULL,
  reported_at_ms BIGINT NOT NULL,
  updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, device_id, identifier)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_property_history (
  id BIGINT NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  identifier VARCHAR(64) NOT NULL,
  value_json JSON NOT NULL,
  reported_at_ms BIGINT NOT NULL,
  msg_id VARCHAR(64) NOT NULL,
  payload_hash CHAR(64) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_property_history (tenant_id, device_id, identifier, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS inbound_dedup (
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  msg_id VARCHAR(64) NOT NULL,
  payload_hash CHAR(64) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, device_id, msg_id, payload_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS inbound_reject (
  id BIGINT NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  topic VARCHAR(255) NOT NULL,
  reason VARCHAR(128) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_reject (tenant_id, device_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS device_command (
  command_id CHAR(36) NOT NULL,
  tenant_id VARCHAR(32) NOT NULL,
  device_id CHAR(12) NOT NULL,
  msg_id VARCHAR(64) NOT NULL,
  state VARCHAR(32) NOT NULL,
  payload LONGBLOB NOT NULL,
  reply_code INT NULL,
  attempts INT NOT NULL DEFAULT 0,
  deadline_at TIMESTAMP(3) NOT NULL,
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (command_id),
  UNIQUE KEY uk_command_msg (tenant_id, device_id, msg_id),
  KEY idx_command_scan (state, deadline_at, created_at),
  CONSTRAINT chk_command_state CHECK (state IN ('pending', 'sending', 'sent', 'accepted', 'failed', 'timeout', 'uncertain', 'needs_attention'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
