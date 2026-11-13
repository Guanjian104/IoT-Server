// Package app 组装配置、日志和 HTTP 服务，并负责优雅退出。
package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"iot-server/internal/api/handler"
	"iot-server/internal/api/router"
	"iot-server/internal/buildinfo"
	"iot-server/internal/config"
	"iot-server/internal/health"
	"iot-server/internal/logger"
)

// Options 是进程装配参数。测试通过 Lookup 和钩子控制副作用。
type Options struct {
	Lookup           config.Lookup
	Logger           logger.Logger
	OnListening      func(addr net.Addr)
	OnBeforeShutdown func()
}

// Run 加载配置并阻塞到 ctx 取消或 HTTP 服务退出。
func Run(ctx context.Context, opt Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := config.Load(opt.Lookup)
	if err != nil {
		return err
	}
	log := opt.Logger
	if log == nil {
		log, err = logger.New(os.Stderr, cfg.LogLevel)
		if err != nil {
			return errors.New("invalid IOT_LOG_LEVEL")
		}
	}
	log.Info("server starting",
		"instance_id", cfg.InstanceID,
		"config_version", cfg.ConfigVersion,
		"build_version", buildinfo.Version,
		"revision", buildinfo.Revision,
	)
	log = logger.Scrub(log, cfg.Secrets())

	rt, err := boot(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer rt.Close()

	state := &health.State{}
	gate := &readyGate{state: state}
	meta := handler.Meta{
		InstanceID:    cfg.InstanceID,
		ConfigVersion: cfg.ConfigVersion,
		BuildVersion:  buildinfo.Version,
		Revision:      buildinfo.Revision,
	}
	server := &http.Server{
		Handler:           router.New(state, meta, rt.Mount),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Error("listen failed", "config_key", "IOT_HTTP_ADDR")
		return errors.New("listen failed")
	}
	listeners := []net.Listener{listener}
	if cfg.HookAddr != "" && cfg.HookAddr != cfg.HTTPAddr {
		hookListener, hookErr := net.Listen("tcp", cfg.HookAddr)
		if hookErr != nil {
			listener.Close()
			log.Error("listen failed", "config_key", "IOT_HOOK_ADDR")
			return errors.New("listen failed")
		}
		listeners = append(listeners, hookListener)
	}

	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		ln := ln
		go func() {
			errCh <- server.Serve(ln)
		}()
	}
	if !cfg.Dependencies() {
		gate.Set(true)
	} else {
		go rt.watch(ctx, gate)
	}
	log.Info("http listening", "addr", listener.Addr().String())
	if opt.OnListening != nil {
		opt.OnListening(listener.Addr())
	}

	select {
	case <-ctx.Done():
		return shutdown(server, gate, cfg.ShutdownTimeout, log, opt.OnBeforeShutdown, errCh, len(listeners))
	case err := <-errCh:
		gate.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped")
			return errors.New("http server stopped")
		}
		return nil
	}
}

func shutdown(server *http.Server, gate *readyGate, timeout time.Duration, log logger.Logger, before func(), errCh <-chan error, listeners int) error {
	gate.Close()
	log.Info("server shutting down")
	if before != nil {
		before()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutErr := server.Shutdown(shutdownCtx)
	var serveErr error
	for i := 0; i < listeners; i++ {
		err := <-errCh
		if err != nil && !errors.Is(err, http.ErrServerClosed) && serveErr == nil {
			serveErr = err
		}
	}
	if shutErr != nil {
		log.Error("shutdown failed")
		return errors.New("shutdown timed out")
	}
	if serveErr != nil {
		log.Error("http server stopped")
		return errors.New("http server stopped")
	}
	log.Info("server stopped")
	return nil
}
