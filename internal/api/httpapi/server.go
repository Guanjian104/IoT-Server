// Package httpapi 提供业务接口和 EMQX 的认证回调。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"iot-server/internal/logger"
	"iot-server/internal/media"
	"iot-server/internal/metrics"
	"iot-server/internal/mqttmsg"
	"iot-server/internal/store"
)

// Kicker 断开一台已经连上 Broker 的客户端。
type Kicker interface {
	Kick(ctx context.Context, clientID string) error
}

// Server 是一组 HTTP 处理函数。
type Server struct {
	store           *store.Store
	log             logger.Logger
	metrics         *metrics.Metrics
	tenant          string
	hookToken       string
	servicePassword string
	devicePassword  string
	deadline        time.Duration
	kicker          Kicker
	media           media.Provider
}

// UseMedia 让观看接口签发这个提供者的凭证。未设置时仍返回不可连接的测试凭证。
func (s *Server) UseMedia(p media.Provider) {
	if s == nil {
		return
	}
	s.media = p
}

// New 组装接口。
func New(st *store.Store, log logger.Logger, m *metrics.Metrics, tenant, hookToken, servicePassword, devicePassword string, deadline time.Duration, kicker Kicker) *Server {
	if deadline <= 0 {
		deadline = 5 * time.Second
	}
	return &Server{
		store:           st,
		log:             log,
		metrics:         m,
		tenant:          tenant,
		hookToken:       hookToken,
		servicePassword: servicePassword,
		devicePassword:  devicePassword,
		deadline:        deadline,
		kicker:          kicker,
	}
}

// Register 把路由挂到已有的探针 mux 上。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /api/v1/devices", s.listDevices)
	mux.HandleFunc("POST /api/v1/devices", s.createDevice)
	mux.HandleFunc("GET /api/v1/devices/{id}", s.getDevice)
	mux.HandleFunc("GET /api/v1/devices/{id}/status", s.getStatus)
	mux.HandleFunc("GET /api/v1/devices/{id}/capabilities", s.getCapabilities)
	mux.HandleFunc("GET /api/v1/devices/{id}/properties", s.getProperties)
	mux.HandleFunc("POST /api/v1/devices/{id}/commands", s.createCommand)
	mux.HandleFunc("GET /api/v1/commands/{commandID}", s.getCommand)
	mux.HandleFunc("POST /api/v1/commands/{commandID}/retry", s.retryCommand)
	mux.HandleFunc("POST /api/v1/commands/{commandID}/close", s.closeCommand)
	mux.HandleFunc("POST /api/v1/devices/{id}/grants", s.putGrant)
	mux.HandleFunc("DELETE /api/v1/devices/{id}/grants", s.deleteGrant)
	mux.HandleFunc("POST /api/v1/devices/{id}/disable", s.disableDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/enable", s.enableDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/revoke", s.revokeDevice)
	mux.HandleFunc("GET /metrics", s.metricsText)
	s.registerRTC(mux)
}

// RegisterHook 只挂内部回调。测试控制入口要单独打开，并且不会出现在业务监听上。
func (s *Server) RegisterHook(mux *http.ServeMux, testControls bool) {
	mux.HandleFunc("POST /internal/mqtt/auth", s.mqttAuth)
	mux.HandleFunc("POST /internal/mqtt/acl", s.mqttACL)
	mux.HandleFunc("POST /internal/media/events", s.mediaEvent)
	if testControls {
		mux.HandleFunc("PUT /internal/rtc/plan", s.putPlan)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) || body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	allowed, err := s.store.Allow(r.Context(), "rl:login:"+body.Username, 20)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	user, err := s.store.Login(r.Context(), s.tenant, body.Username, body.Password)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	token, expires, err := s.store.IssueToken(r.Context(), user)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expires.Format(time.RFC3339),
		"user": map[string]any{
			"id":             user.ID,
			"username":       user.Username,
			"tenant_id":      user.TenantID,
			"platform_admin": user.PlatformAdmin,
		},
	})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	items, err := s.store.ListDevices(r.Context(), user)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devicesJSON(items)})
}

func (s *Server) createDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if !user.PlatformAdmin {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		DeviceID  string `json:"device_id"`
		ProductID string `json:"product_id"`
	}
	if !decode(w, r, &body) || !mqttmsg.ValidDeviceID(body.DeviceID) || body.ProductID == "" {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	err := s.store.CreateDevice(r.Context(), user.TenantID, body.DeviceID, body.ProductID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.log.Info("device created", "request_id", requestID(r), "device_id", body.DeviceID)
	writeJSON(w, http.StatusCreated, map[string]any{"device_id": body.DeviceID, "product_id": body.ProductID})
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	s.withDevice(w, r, "read", func(user store.User, device store.Device) {
		writeJSON(w, http.StatusOK, deviceJSON(device))
	})
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	s.withDevice(w, r, "read", func(user store.User, device store.Device) {
		status, err := s.store.GetStatus(r.Context(), user.TenantID, device.DeviceID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"device_id":          status.DeviceID,
			"mqtt_online":        status.MQTTOnline,
			"rtc_connected":      status.RTCConnected,
			"track_ready":        status.TrackReady,
			"reported_status":    status.ReportedStatus,
			"generation_ms":      status.GenerationMs,
			"connected_at_ms":    status.ConnectedAtMs,
			"disconnected_at_ms": status.DisconnectedAtMs,
		})
	})
}

func (s *Server) getCapabilities(w http.ResponseWriter, r *http.Request) {
	s.withDevice(w, r, "read", func(user store.User, device store.Device) {
		items, err := s.store.Capabilities(r.Context(), user.TenantID, device.DeviceID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			out = append(out, map[string]any{
				"identifier": item.Identifier,
				"name":       item.Name,
				"data_type":  item.DataType,
				"writable":   item.Writable,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"capabilities": out})
	})
}

func (s *Server) getProperties(w http.ResponseWriter, r *http.Request) {
	s.withDevice(w, r, "read", func(user store.User, device store.Device) {
		items, err := s.store.Properties(r.Context(), user.TenantID, device.DeviceID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			out = append(out, map[string]any{
				"identifier":     item.Identifier,
				"value":          item.Value,
				"reported_at_ms": item.ReportedAtMs,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"current": out})
	})
}

func (s *Server) createCommand(w http.ResponseWriter, r *http.Request) {
	s.withDevice(w, r, "manage", func(user store.User, device store.Device) {
		if !device.Enabled || device.Revoked {
			writeError(w, http.StatusConflict, "conflict")
			return
		}
		allowed, err := s.store.Allow(r.Context(), "rl:cmd:"+strconv.FormatInt(user.ID, 10), 30)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if !allowed {
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		var body struct {
			Identifier string `json:"identifier"`
			Value      *bool  `json:"value"`
		}
		if !decode(w, r, &body) || body.Value == nil || !mqttmsg.IsSwitch(body.Identifier) {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		command, err := s.store.Enqueue(r.Context(), user.TenantID, device.DeviceID, body.Identifier, *body.Value, time.Now().Add(s.deadline))
		if errors.Is(err, store.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		s.log.Info("command queued", "request_id", requestID(r), "command_id", command.ID, "device_id", device.DeviceID, "msg_id", command.MsgID)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"command_id": command.ID,
			"device_id":  command.DeviceID,
			"msg_id":     command.MsgID,
			"state":      command.State,
		})
	})
}

func (s *Server) getCommand(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	commandID := r.PathValue("commandID")
	command, err := s.store.GetCommand(r.Context(), user.TenantID, commandID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !s.allow(w, r, user, command.DeviceID, "read") {
		return
	}
	writeJSON(w, http.StatusOK, commandBody(command))
}

func (s *Server) retryCommand(w http.ResponseWriter, r *http.Request) {
	s.recoverCommand(w, r, true)
}

func (s *Server) closeCommand(w http.ResponseWriter, r *http.Request) {
	s.recoverCommand(w, r, false)
}

// recoverCommand 处理不确定或需要人工介入的命令。
// retry 会再次发布同一条载荷；close 收成 failed，表示人工确认不再执行。
func (s *Server) recoverCommand(w http.ResponseWriter, r *http.Request, retry bool) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	commandID := r.PathValue("commandID")
	current, err := s.store.GetCommand(r.Context(), user.TenantID, commandID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !s.allow(w, r, user, current.DeviceID, "manage") {
		return
	}
	var command store.Command
	if retry {
		command, err = s.store.RetryCommand(r.Context(), user.TenantID, commandID, time.Now().Add(s.deadline))
	} else {
		command, err = s.store.CloseCommand(r.Context(), user.TenantID, commandID)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	action := "close"
	if retry {
		action = "retry"
	}
	s.log.Info("command recovered", "request_id", requestID(r), "command_id", command.ID, "action", action, "state", command.State)
	writeJSON(w, http.StatusOK, commandBody(command))
}

func (s *Server) putGrant(w http.ResponseWriter, r *http.Request) {
	s.changeGrant(w, r, true)
}

func (s *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	s.changeGrant(w, r, false)
}

func (s *Server) changeGrant(w http.ResponseWriter, r *http.Request, add bool) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if !user.PlatformAdmin {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	deviceID := r.PathValue("id")
	if !mqttmsg.ValidDeviceID(deviceID) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	var body struct {
		Username   string `json:"username"`
		Permission string `json:"permission"`
	}
	if !decode(w, r, &body) || body.Username == "" || !validPermission(body.Permission) {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	var err error
	if add {
		err = s.store.PutGrant(r.Context(), user.TenantID, body.Username, deviceID, body.Permission)
	} else {
		err = s.store.DeleteGrant(r.Context(), user.TenantID, body.Username, deviceID, body.Permission)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_id": deviceID, "username": body.Username, "permission": body.Permission})
}

func (s *Server) disableDevice(w http.ResponseWriter, r *http.Request) {
	s.setAccess(w, r, func(ctx context.Context, tenant, deviceID string) error {
		return s.store.SetEnabled(ctx, tenant, deviceID, false)
	}, true)
}

func (s *Server) enableDevice(w http.ResponseWriter, r *http.Request) {
	s.setAccess(w, r, func(ctx context.Context, tenant, deviceID string) error {
		return s.store.SetEnabled(ctx, tenant, deviceID, true)
	}, false)
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	s.setAccess(w, r, func(ctx context.Context, tenant, deviceID string) error {
		return s.store.SetRevoked(ctx, tenant, deviceID, true)
	}, true)
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request, fn func(context.Context, string, string) error, kick bool) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if !user.PlatformAdmin {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	deviceID := r.PathValue("id")
	if !mqttmsg.ValidDeviceID(deviceID) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err := fn(r.Context(), user.TenantID, deviceID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	body := map[string]any{"device_id": deviceID}
	if kick {
		result := s.kick(r.Context(), deviceID)
		body["kick"] = result
		if s.log != nil {
			s.log.Info("device access changed", "request_id", requestID(r), "device_id", deviceID, "kick", result)
		}
	} else if s.log != nil {
		s.log.Info("device access changed", "request_id", requestID(r), "device_id", deviceID)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) kick(ctx context.Context, deviceID string) string {
	if s.kicker == nil {
		return "skipped"
	}
	if err := s.kicker.Kick(ctx, deviceID); err != nil {
		if s.metrics != nil {
			s.metrics.NoteKick(false)
		}
		if s.log != nil {
			s.log.Warn("device kick failed", "device_id", deviceID)
		}
		return "failed"
	}
	if s.metrics != nil {
		s.metrics.NoteKick(true)
	}
	return "ok"
}

func (s *Server) metricsText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, s.metrics.Text())
}

func (s *Server) withDevice(w http.ResponseWriter, r *http.Request, perm string, fn func(store.User, store.Device)) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	deviceID := r.PathValue("id")
	if !mqttmsg.ValidDeviceID(deviceID) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	device, err := s.store.GetDevice(r.Context(), user.TenantID, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !s.allow(w, r, user, device.DeviceID, perm) {
		return
	}
	fn(user, device)
}

func (s *Server) allow(w http.ResponseWriter, r *http.Request, user store.User, deviceID, perm string) bool {
	if user.PlatformAdmin {
		return true
	}
	grants, err := s.store.Grants(r.Context(), user.TenantID, user.ID, deviceID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return false
	}
	if perm == "read" && (grants["view"] || grants["manage"]) {
		return true
	}
	if grants[perm] {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden")
	return false
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" || strings.Contains(token, " ") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.User{}, false
	}
	user, err := s.store.UserByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.User{}, false
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return store.User{}, false
	}
	return user, true
}

func validPermission(value string) bool {
	switch value {
	case "manage", "view", "call", "ticket":
		return true
	default:
		return false
	}
}

func devicesJSON(items []store.Device) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, deviceJSON(item))
	}
	return out
}

func commandBody(command store.Command) map[string]any {
	body := map[string]any{
		"command_id": command.ID,
		"device_id":  command.DeviceID,
		"msg_id":     command.MsgID,
		"state":      command.State,
		"attempts":   command.Attempts,
	}
	if command.ReplyCode != nil {
		body["reply_code"] = *command.ReplyCode
	}
	return body
}

func deviceJSON(device store.Device) map[string]any {
	return map[string]any{
		"device_id":          device.DeviceID,
		"product_id":         device.Product,
		"enabled":            device.Enabled,
		"credential_revoked": device.Revoked,
	}
}

func decode(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 64 {
		return id
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
