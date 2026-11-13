// Package handler 只负责 HTTP 协议转换。
// B02 没有业务 Service 和 Repository。
package handler

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"

	"iot-server/internal/buildinfo"
	"iot-server/internal/health"
)

// Meta 是可查询的实例、配置和构建标识。
type Meta struct {
	InstanceID    string
	ConfigVersion string
	BuildVersion  string
	Revision      string
}

// Live 返回存活探针。请求能到达进程即视为存活。
func Live(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "live"})
}

// Ready 返回就绪探针。关闭过程中返回 503。
func Ready(state *health.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowRead(w, r) {
			return
		}
		if state != nil && state.Ready() {
			writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
	}
}

// Version 返回实例标识、配置版本和构建版本。
func Version(meta Meta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowRead(w, r) {
			return
		}
		buildVersion := meta.BuildVersion
		if buildVersion == "" {
			buildVersion = buildinfo.Version
		}
		revision := meta.Revision
		if revision == "" {
			revision = buildinfo.Revision
		}
		writeJSON(w, r, http.StatusOK, map[string]string{
			"instance_id":    meta.InstanceID,
			"config_version": meta.ConfigVersion,
			"build_version":  buildVersion,
			"revision":       revision,
			"go_version":     runtime.Version(),
		})
	}
}

func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, r, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	return false
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body map[string]string) {
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
	if r.Method == http.MethodHead {
		w.Header().Set("Connection", "close")
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(payload)
}
