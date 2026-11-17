ALTER TABLE rtc_task ADD COLUMN lease_token BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS mqtt_inbox (
  tenant_id VARCHAR(32) NOT NULL,
  msg_hash CHAR(64) NOT NULL,
  topic VARCHAR(255) NOT NULL,
  payload MEDIUMBLOB NOT NULL,
  attempts INT NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL,
  last_error VARCHAR(64) NOT NULL DEFAULT '',
  created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (tenant_id, msg_hash),
  KEY idx_inbox_status (status, updated_at),
  CONSTRAINT chk_inbox_status CHECK (status IN ('pending', 'poison', 'done'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
