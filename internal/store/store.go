// Package store 把业务状态放在 MySQL，把短期限流放在 Redis。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"iot-server/internal/config"
	"iot-server/internal/logger"
	"iot-server/internal/media"
)

var (
	// ErrNotFound 表示租户内没有这条记录。
	ErrNotFound = errors.New("not found")
	// ErrConflict 表示唯一约束冲突或设备当前不可操作。
	ErrConflict = errors.New("conflict")
	// ErrUnavailable 表示依赖暂时不可用。调用方应拒绝，而不是放行。
	ErrUnavailable = errors.New("unavailable")
	// ErrBusy 表示设备主通话占用已被持有。
	ErrBusy = errors.New("busy")
	// ErrStopping 表示当前流正在停止或结果未知，不能开启新轮次。
	ErrStopping = errors.New("stopping")
	// ErrForbidden 表示用户无权执行这个操作。
	ErrForbidden = errors.New("forbidden")
	// ErrTicket 表示票据不存在、已换票或已过期。
	ErrTicket = errors.New("ticket")
	// ErrInvalid 表示调用方给出的维度不合法。
	ErrInvalid = errors.New("invalid")
)

// Store 是业务表的访问入口。
type Store struct {
	db            *sql.DB
	rdb           *redis.Client
	log           logger.Logger
	stopDelay     time.Duration
	joinGrace     time.Duration
	ringTimeout   time.Duration
	taskLease     time.Duration
	mediaProvider media.Provider
}

// UseMedia 让开流和结束使用这个媒体提供者。未设置时仍使用测试替身。
func (s *Store) UseMedia(p media.Provider) {
	if s == nil {
		return
	}
	s.mediaProvider = p
}

// Open 打开 MySQL 连接池。网络失败留到 Ping，不在这里阻塞。
func Open(cfg config.Config) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true&loc=UTC&charset=utf8mb4&collation=utf8mb4_0900_ai_ci&timeout=3s&readTimeout=5s&writeTimeout=5s",
		url.UserPassword(cfg.MySQLUser, cfg.MySQLPassword).String(), cfg.MySQLAddr, cfg.MySQLDatabase)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.New("datastore open failed")
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// New 组装存储。Redis 可以为空，此时限流视为依赖失败。
func New(db *sql.DB, rdb *redis.Client, log logger.Logger) *Store {
	return &Store{db: db, rdb: rdb, log: log}
}

func (s *Store) unavailable(op string, err error) error {
	if s.log != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			s.log.Error("db operation failed", "operation", op, "mysql_code", me.Number)
		} else {
			s.log.Debug("db operation failed", "operation", op)
		}
	}
	return fmt.Errorf("%w", ErrUnavailable)
}

// Allow 做固定窗口限流。Redis 不可用时返回错误，由调用方拒绝请求。
func (s *Store) Allow(ctx context.Context, key string, limit int) (bool, error) {
	if s.rdb == nil {
		return false, fmt.Errorf("%w", ErrUnavailable)
	}
	count, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, s.unavailable("rate_limit", err)
	}
	if count == 1 {
		if err := s.rdb.Expire(ctx, key, time.Minute).Err(); err != nil {
			return false, s.unavailable("rate_limit", err)
		}
	}
	return count <= int64(limit), nil
}
