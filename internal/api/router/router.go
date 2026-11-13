// Package router 组装骨架的 HTTP 路由。
package router

import (
	"net/http"

	"iot-server/internal/api/handler"
	"iot-server/internal/health"
)

// New 注册存活、就绪和版本探针，并可挂上业务路由。
func New(state *health.State, meta handler.Meta, mount func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handler.Live)
	mux.HandleFunc("HEAD /health/live", handler.Live)
	mux.HandleFunc("GET /health/ready", handler.Ready(state))
	mux.HandleFunc("HEAD /health/ready", handler.Ready(state))
	mux.HandleFunc("GET /version", handler.Version(meta))
	mux.HandleFunc("HEAD /version", handler.Version(meta))
	if mount != nil {
		mount(mux)
	}
	return mux
}
