package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"iot-server/internal/mqttmsg"
)

// Device 是租户内的一台设备。
type Device struct {
	DeviceID   string
	Product    string
	TopicModel string
	Enabled    bool
	Revoked    bool
}

// Capability 是设备上的一个功能点。
type Capability struct {
	Identifier string
	Name       string
	DataType   string
	Writable   bool
}

// Property 是当前属性值。Value 为 bool 或 string。
type Property struct {
	Identifier   string
	Value        any
	ReportedAtMs int64
}

// Status 把 MQTT 会话、设备自报状态和 RTC 观察分开返回。
type Status struct {
	DeviceID         string
	MQTTOnline       bool
	RTCConnected     bool
	TrackReady       bool
	ReportedStatus   string
	GenerationMs     int64
	ConnectedAtMs    int64
	DisconnectedAtMs int64
}

// DeviceGate 是 MQTT 认证使用的启用状态。
type DeviceGate struct {
	Found      bool
	Enabled    bool
	Revoked    bool
	TopicModel string
}

// CreateDevice 登记一台设备，并写入当前固件的全部功能点。
func (s *Store) CreateDevice(ctx context.Context, tenant, deviceID, productID, topicModel string) error {
	if !mqttmsg.KnownModel(topicModel) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.unavailable("create_device", err)
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM product WHERE tenant_id = ? AND product_id = ?`, tenant, productID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return s.unavailable("create_device", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO device (tenant_id, device_id, product_id, topic_model, enabled, credential_revoked)
		VALUES (?, ?, ?, ?, 1, 0)`, tenant, deviceID, productID, topicModel)
	if err != nil {
		if isDuplicate(err) {
			return ErrConflict
		}
		return s.unavailable("create_device", err)
	}
	for _, point := range mqttmsg.Points {
		writable := 0
		if point.Writable {
			writable = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO device_capability (tenant_id, device_id, identifier, display_name, data_type, writable)
			VALUES (?, ?, ?, ?, ?, ?)`, tenant, deviceID, point.Identifier, point.Name, point.DataType, writable); err != nil {
			return s.unavailable("create_device", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_presence (tenant_id, device_id) VALUES (?, ?)`, tenant, deviceID); err != nil {
		return s.unavailable("create_device", err)
	}
	return tx.Commit()
}

// GetDevice 按租户读取设备。
func (s *Store) GetDevice(ctx context.Context, tenant, deviceID string) (Device, error) {
	var device Device
	var enabled, revoked int
	err := s.db.QueryRowContext(ctx, `
		SELECT device_id, product_id, topic_model, enabled, credential_revoked
		FROM device WHERE tenant_id = ? AND device_id = ?`, tenant, deviceID).Scan(&device.DeviceID, &device.Product, &device.TopicModel, &enabled, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, s.unavailable("get_device", err)
	}
	device.Enabled = enabled == 1
	device.Revoked = revoked == 1
	return device, nil
}

// DeviceGate 读取认证需要的启用和吊销状态。没有记录时 Found 为 false，且不是依赖错误。
func (s *Store) DeviceGate(ctx context.Context, tenant, deviceID string) (DeviceGate, error) {
	device, err := s.GetDevice(ctx, tenant, deviceID)
	if errors.Is(err, ErrNotFound) {
		return DeviceGate{}, nil
	}
	if err != nil {
		return DeviceGate{}, err
	}
	return DeviceGate{Found: true, Enabled: device.Enabled, Revoked: device.Revoked, TopicModel: device.TopicModel}, nil
}

// ListDevices 列出调用方可见的设备。平台管理员看本租户全部，其余用户只看已授权设备。
func (s *Store) ListDevices(ctx context.Context, user User) ([]Device, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if user.PlatformAdmin {
		rows, err = s.db.QueryContext(ctx, `
			SELECT device_id, product_id, topic_model, enabled, credential_revoked
			FROM device WHERE tenant_id = ? ORDER BY device_id`, user.TenantID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT DISTINCT d.device_id, d.product_id, d.topic_model, d.enabled, d.credential_revoked
			FROM device d
			JOIN user_device_grant g ON g.tenant_id = d.tenant_id AND g.device_id = d.device_id AND g.user_id = ?
			WHERE d.tenant_id = ? AND g.permission IN ('view', 'manage')
			ORDER BY d.device_id`, user.ID, user.TenantID)
	}
	if err != nil {
		return nil, s.unavailable("list_devices", err)
	}
	defer rows.Close()
	out, err := scanDevices(rows)
	if err != nil {
		return nil, s.unavailable("list_devices", err)
	}
	return out, nil
}

func scanDevices(rows *sql.Rows) ([]Device, error) {
	var out []Device
	for rows.Next() {
		var device Device
		var enabled, revoked int
		if err := rows.Scan(&device.DeviceID, &device.Product, &device.TopicModel, &enabled, &revoked); err != nil {
			return nil, err
		}
		device.Enabled = enabled == 1
		device.Revoked = revoked == 1
		out = append(out, device)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Device{}
	}
	return out, nil
}

// Capabilities 返回设备功能点。
func (s *Store) Capabilities(ctx context.Context, tenant, deviceID string) ([]Capability, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identifier, display_name, data_type, writable
		FROM device_capability WHERE tenant_id = ? AND device_id = ? ORDER BY identifier`, tenant, deviceID)
	if err != nil {
		return nil, s.unavailable("capabilities", err)
	}
	defer rows.Close()
	var out []Capability
	for rows.Next() {
		var item Capability
		var writable int
		if err := rows.Scan(&item.Identifier, &item.Name, &item.DataType, &writable); err != nil {
			return nil, s.unavailable("capabilities", err)
		}
		item.Writable = writable == 1
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("capabilities", err)
	}
	if out == nil {
		out = []Capability{}
	}
	return out, nil
}

// SetTopicModel 修改设备使用的主题代次。未知代次视为设备不存在，避免把非法值写进库。
func (s *Store) SetTopicModel(ctx context.Context, tenant, deviceID, topicModel string) error {
	if !mqttmsg.KnownModel(topicModel) {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE device SET topic_model = ? WHERE tenant_id = ? AND device_id = ?`, topicModel, tenant, deviceID)
	if err != nil {
		return s.unavailable("topic_model", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.unavailable("topic_model", err)
	}
	if n > 0 {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM device WHERE tenant_id = ? AND device_id = ?`, tenant, deviceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return s.unavailable("topic_model", err)
	}
	return nil
}

// SetEnabled 启用或停用设备。
func (s *Store) SetEnabled(ctx context.Context, tenant, deviceID string, enabled bool) error {
	return s.setFlag(ctx, tenant, deviceID, "enabled", enabled)
}

// SetRevoked 吊销或恢复设备接入资格。
func (s *Store) SetRevoked(ctx context.Context, tenant, deviceID string, revoked bool) error {
	return s.setFlag(ctx, tenant, deviceID, "credential_revoked", revoked)
}

func (s *Store) setFlag(ctx context.Context, tenant, deviceID, column string, on bool) error {
	value := 0
	if on {
		value = 1
	}
	query := `UPDATE device SET ` + column + ` = ? WHERE tenant_id = ? AND device_id = ?`
	if column != "enabled" && column != "credential_revoked" {
		return s.unavailable("device_flag", errors.New("column"))
	}
	res, err := s.db.ExecContext(ctx, query, value, tenant, deviceID)
	if err != nil {
		return s.unavailable("device_flag", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.unavailable("device_flag", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Grants 返回某个用户对某台设备的权限。
func (s *Store) Grants(ctx context.Context, tenant string, userID int64, deviceID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT permission FROM user_device_grant
		WHERE tenant_id = ? AND user_id = ? AND device_id = ?`, tenant, userID, deviceID)
	if err != nil {
		return nil, s.unavailable("grants", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var perm string
		if err := rows.Scan(&perm); err != nil {
			return nil, s.unavailable("grants", err)
		}
		out[perm] = true
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("grants", err)
	}
	return out, nil
}

// PutGrant 给同租户用户增加一项权限。
func (s *Store) PutGrant(ctx context.Context, tenant, username, deviceID, permission string) error {
	user, err := s.userByName(ctx, tenant, username)
	if err != nil {
		return err
	}
	if _, err := s.GetDevice(ctx, tenant, deviceID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT IGNORE INTO user_device_grant (tenant_id, user_id, device_id, permission)
		VALUES (?, ?, ?, ?)`, tenant, user.ID, deviceID, permission)
	if err != nil {
		return s.unavailable("put_grant", err)
	}
	return nil
}

// DeleteGrant 删除一项权限。
func (s *Store) DeleteGrant(ctx context.Context, tenant, username, deviceID, permission string) error {
	user, err := s.userByName(ctx, tenant, username)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM user_device_grant
		WHERE tenant_id = ? AND user_id = ? AND device_id = ? AND permission = ?`,
		tenant, user.ID, deviceID, permission)
	if err != nil {
		return s.unavailable("delete_grant", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.unavailable("delete_grant", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) userByName(ctx context.Context, tenant, username string) (User, error) {
	var user User
	var admin int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, username, platform_admin, status
		FROM app_user WHERE tenant_id = ? AND username = ?`, tenant, username).Scan(
		&user.ID, &user.TenantID, &user.Username, &admin, &user.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, s.unavailable("user_lookup", err)
	}
	user.PlatformAdmin = admin == 1
	return user, nil
}

func isDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate")
}
