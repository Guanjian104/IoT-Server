package store

import (
	"context"

	"golang.org/x/crypto/bcrypt"
	"iot-server/internal/mqttmsg"
)

const sampleDeviceID = "94a990cb56a8"

// Seed 写入开发租户的产品和样例设备。用户只在提供了口令时创建，重复启动不改已有口令。
func (s *Store) Seed(ctx context.Context, tenant, adminPassword, operatorPassword string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO product (tenant_id, product_id, name) VALUES (?, 'controller', 'controller'), (?, 'collar', 'collar')`, tenant, tenant); err != nil {
		return s.unavailable("seed", err)
	}
	if err := s.seedDevice(ctx, tenant, sampleDeviceID); err != nil {
		return err
	}
	if adminPassword != "" {
		if err := s.seedUser(ctx, tenant, "admin", adminPassword, true); err != nil {
			return err
		}
		if err := s.seedGrants(ctx, tenant, "admin", sampleDeviceID, "manage", "view", "call", "ticket"); err != nil {
			return err
		}
	}
	if operatorPassword != "" {
		if err := s.seedUser(ctx, tenant, "operator", operatorPassword, false); err != nil {
			return err
		}
		if err := s.seedGrants(ctx, tenant, "operator", sampleDeviceID, "view"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) seedDevice(ctx context.Context, tenant, deviceID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO device (tenant_id, device_id, product_id, enabled, credential_revoked)
		VALUES (?, ?, 'controller', 1, 0)`, tenant, deviceID)
	if err != nil {
		return s.unavailable("seed", err)
	}
	for _, point := range mqttmsg.Points {
		writable := 0
		if point.Writable {
			writable = 1
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO device_capability (tenant_id, device_id, identifier, display_name, data_type, writable)
			VALUES (?, ?, ?, ?, ?, ?)`, tenant, deviceID, point.Identifier, point.Name, point.DataType, writable); err != nil {
			return s.unavailable("seed", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO device_presence (tenant_id, device_id) VALUES (?, ?)`, tenant, deviceID); err != nil {
		return s.unavailable("seed", err)
	}
	return nil
}

func (s *Store) seedUser(ctx context.Context, tenant, username, password string, admin bool) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return s.unavailable("seed", err)
	}
	flag := 0
	if admin {
		flag = 1
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO app_user (tenant_id, username, password_hash, platform_admin, status)
		VALUES (?, ?, ?, ?, 'active')
		ON DUPLICATE KEY UPDATE id = id`, tenant, username, string(hash), flag)
	if err != nil {
		return s.unavailable("seed", err)
	}
	return nil
}

func (s *Store) seedGrants(ctx context.Context, tenant, username, deviceID string, permissions ...string) error {
	user, err := s.userByName(ctx, tenant, username)
	if err != nil {
		return err
	}
	for _, permission := range permissions {
		if _, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO user_device_grant (tenant_id, user_id, device_id, permission)
			VALUES (?, ?, ?, ?)`, tenant, user.ID, deviceID, permission); err != nil {
			return s.unavailable("seed", err)
		}
	}
	return nil
}
