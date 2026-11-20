package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"iot-server/internal/api/httpapi"
	"iot-server/internal/bridge"
	"iot-server/internal/buildinfo"
	"iot-server/internal/config"
	"iot-server/internal/emqx"
	"iot-server/internal/health"
	"iot-server/internal/logger"
	"iot-server/internal/metrics"
	"iot-server/internal/migrate"
	"iot-server/internal/sfu"
	"iot-server/internal/store"
)

type runtime struct {
	cfg       config.Config
	log       logger.Logger
	db        *sql.DB
	rdb       *redis.Client
	store     *store.Store
	bridge    *bridge.Bridge
	kicker    *emqx.Client
	metrics   *metrics.Metrics
	sfu       *sfu.Node
	migrated  bool
	attention int
	once      sync.Once
	workOnce  sync.Once
}

func boot(ctx context.Context, cfg config.Config, log logger.Logger) (*runtime, error) {
	rt := &runtime{
		cfg:     cfg,
		log:     log,
		metrics: metrics.New(buildinfo.Version, buildinfo.Revision, cfg.InstanceID),
	}
	if !cfg.Dependencies() {
		if err := rt.openSFU(); err != nil {
			return nil, err
		}
		return rt, nil
	}
	if cfg.MySQLOn() {
		db, err := store.Open(cfg)
		if err != nil {
			return nil, err
		}
		rt.db = db
		rt.store = store.New(db, nil, log)
	}
	if cfg.RedisOn() {
		rt.rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
		if rt.db != nil {
			rt.store = store.New(rt.db, rt.rdb, log)
		}
	}
	if rt.store != nil {
		rt.store.ConfigureRTC(cfg.StopDelay, cfg.JoinGrace, cfg.RingTimeout)
	}
	if cfg.MQTTOn() {
		rt.kicker = emqx.New(cfg.EMQXAPIURL, cfg.EMQXAPIUser, cfg.EMQXAPIPassword)
		if rt.store != nil {
			rt.bridge = bridge.New(ctx, cfg, rt.store, log, rt.metrics)
			rt.bridge.UseSessions(rt.kicker)
		}
	}
	if err := rt.openSFU(); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

func (rt *runtime) openSFU() error {
	if rt.cfg.SFUAddr == "" {
		return nil
	}
	node, err := sfu.ListenEndpoint(rt.cfg.SFUAddr, sfu.Endpoint{
		PublicIP:  rt.cfg.SFUPublicIP,
		PublicURL: rt.cfg.SFUPublicURL,
		UDPMin:    uint16(rt.cfg.SFUUDPMin),
		UDPMax:    uint16(rt.cfg.SFUUDPMax),
	})
	if err != nil {
		if rt.log != nil {
			rt.log.Error("listen failed", "config_key", "IOT_SFU_ADDR")
		}
		return errors.New("listen failed")
	}
	rt.sfu = node
	if rt.store != nil {
		rt.store.UseMedia(node.Media())
	}
	if rt.log != nil {
		rt.log.Info("sfu listening", "addr", rt.cfg.SFUAddr)
	}
	return nil
}

func (rt *runtime) Mount(mux *http.ServeMux) {
	api := rt.api()
	if api == nil {
		return
	}
	api.Register(mux)
}

func (rt *runtime) MountHook(mux *http.ServeMux) {
	api := rt.api()
	if api == nil {
		return
	}
	api.RegisterHook(mux, rt.cfg.EnableTestControls)
}

func (rt *runtime) api() *httpapi.Server {
	if rt == nil || rt.store == nil {
		return nil
	}
	api := httpapi.New(rt.store, rt.log, rt.metrics, rt.cfg.TenantID, rt.cfg.MQTTHookToken, rt.cfg.MQTTServicePass, rt.cfg.MQTTDevicePass, rt.cfg.CommandDeadline, rt.kicker)
	if rt.sfu != nil {
		api.UseMedia(rt.sfu.Media())
	}
	return api
}

func (rt *runtime) watch(ctx context.Context, gate *readyGate) {
	rt.sync(ctx, gate)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.sync(ctx, gate)
		}
	}
}

func (rt *runtime) sync(ctx context.Context, gate *readyGate) {
	if ctx.Err() != nil {
		return
	}
	mysqlOK := !rt.cfg.MySQLOn()
	if rt.cfg.MySQLOn() && rt.db != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := rt.db.PingContext(pingCtx)
		cancel()
		mysqlOK = err == nil
		if mysqlOK && !rt.migrated {
			migrateCtx, migrateCancel := context.WithTimeout(ctx, 30*time.Second)
			err = migrate.Up(migrateCtx, rt.db)
			if err == nil && rt.store != nil {
				err = rt.store.Seed(migrateCtx, rt.cfg.TenantID, rt.cfg.SeedAdminPass, rt.cfg.SeedOperatorPass)
			}
			migrateCancel()
			if err != nil {
				mysqlOK = false
				if rt.log != nil {
					rt.log.Error("schema sync failed")
				}
			} else {
				rt.migrated = true
				rt.startRTC(ctx)
			}
		}
		if rt.cfg.MySQLOn() && !rt.migrated {
			mysqlOK = false
		}
	}
	redisOK := !rt.cfg.RedisOn()
	if rt.cfg.RedisOn() && rt.rdb != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := rt.rdb.Ping(pingCtx).Err()
		cancel()
		redisOK = err == nil
	}
	mqttOK := !rt.cfg.MQTTOn()
	mqttSubscribed := !rt.cfg.MQTTOn()
	if rt.cfg.MQTTOn() && rt.bridge != nil && rt.migrated {
		rt.bridge.Ensure()
		mqttSubscribed = rt.bridge.Ready()
		mqttOK = mqttSubscribed
	}
	attention := rt.attention
	if mysqlOK && rt.store != nil {
		countCtx, countCancel := context.WithTimeout(ctx, time.Second)
		n, err := rt.store.CommandAttention(countCtx, rt.cfg.TenantID)
		countCancel()
		if err == nil {
			attention = n
			rt.attention = n
		}
	}
	if rt.metrics != nil {
		rt.metrics.SetDependency("mysql", mysqlOK)
		rt.metrics.SetDependency("redis", redisOK)
		rt.metrics.SetDependency("mqtt", mqttOK)
		rt.metrics.SetCommandAttention(attention)
	}
	gate.Publish(health.Report{Ready: mysqlOK && redisOK && mqttOK, MQTTSubscribed: mqttSubscribed, CommandAttention: attention})
}

func (rt *runtime) startRTC(ctx context.Context) {
	if rt.store == nil {
		return
	}
	rt.workOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := rt.store.MaintainRTC(ctx, rt.cfg.InstanceID); err != nil && rt.log != nil {
						rt.log.Error("rtc maintain failed")
					}
				}
			}
		}()
	})
}

func (rt *runtime) Close() {
	if rt == nil {
		return
	}
	rt.once.Do(func() {
		if rt.sfu != nil {
			_ = rt.sfu.Close()
		}
		if rt.bridge != nil {
			rt.bridge.Close()
		}
		if rt.rdb != nil {
			_ = rt.rdb.Close()
		}
		if rt.db != nil {
			_ = rt.db.Close()
		}
	})
}

type readyGate struct {
	mu     sync.Mutex
	closed bool
	state  *health.State
}

func (g *readyGate) Set(ready bool) {
	g.Publish(health.Report{Ready: ready, MQTTSubscribed: ready})
}

func (g *readyGate) Publish(report health.Report) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.state.Publish(report)
}

func (g *readyGate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.state.SetReady(false)
}
