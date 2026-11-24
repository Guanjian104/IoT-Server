-- 把 EMQX 内置账号表和允许表挪到 eco_iot。
-- 口令散列不写在这个文件里，由 scripts/copy-emqx-credentials.py 从正在使用的内置表抄过来。
-- emqx_gate 原来只能读 device。这两张新表也只给它读。

CREATE TABLE IF NOT EXISTS mqtt_credential (
  username VARCHAR(64) NOT NULL,
  password_hash CHAR(64) NOT NULL,
  salt VARCHAR(64) NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS mqtt_acl (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  username VARCHAR(64) NOT NULL,
  clientid VARCHAR(128) NOT NULL DEFAULT '',
  permission VARCHAR(8) NOT NULL,
  action VARCHAR(16) NOT NULL,
  topic VARCHAR(255) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mqtt_acl (username, clientid, permission, action, topic)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE mqtt_credential CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
ALTER TABLE mqtt_acl CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

INSERT IGNORE INTO mqtt_acl (username, clientid, permission, action, topic) VALUES
  ('eco-iot', '', 'allow', 'subscribe', 'cat/event'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/ack'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/archive'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/enter'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/leave'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/scene'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/status'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/+/telemetry'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/event/petcollar/+'),
  ('eco-iot', '', 'allow', 'subscribe', 'device/telemetry/petcollar/v2/+'),
  ('eco-iot', '', 'allow', 'subscribe', 'horse/collar/+/state'),
  ('eco-iot', '', 'allow', 'subscribe', 'pet/collar/+/state'),
  ('eco-iot', '', 'allow', 'subscribe', 'pet/collar/1_0pro/+/state'),
  ('eco-iot', '', 'allow', 'subscribe', 'pet/collar/2_0_test/+/state'),
  ('eco-iot', '', 'allow', 'publish', 'device/+/cmd'),
  ('eco-iot', '', 'allow', 'publish', 'device/cmd/petcollar/+'),
  ('eco-iot', '', 'allow', 'publish', 'device/+/archive/clear'),
  ('root', '9030d6cb0305', 'allow', 'subscribe', 'pet/collar/+/+/state'),
  ('root', '9030d6cb0305', 'allow', 'publish', 'pet/collar/1_0pro/+/cmd'),
  ('root', '9030d6cb0305', 'allow', 'publish', 'pet/collar/2_0_test/+/cmd');

GRANT SELECT ON eco_iot.mqtt_credential TO 'emqx_gate'@'%';
GRANT SELECT ON eco_iot.mqtt_acl TO 'emqx_gate'@'%';
