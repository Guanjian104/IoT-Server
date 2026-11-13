package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const tokenTTL = 12 * time.Hour

// User 是已验证的本地用户。请求体里的 user_id 不能变成这个结构。
type User struct {
	ID            int64
	TenantID      string
	Username      string
	PlatformAdmin bool
	Status        string
}

// Login 只接受用户名和密码。用户不存在和密码错误都表现为未找到，避免把哈希细节返回给接口层。
func (s *Store) Login(ctx context.Context, tenant, username, password string) (User, error) {
	var user User
	var hash string
	var admin int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, username, password_hash, platform_admin, status
		FROM app_user WHERE tenant_id = ? AND username = ?`, tenant, username).Scan(
		&user.ID, &user.TenantID, &user.Username, &hash, &admin, &user.Status)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, s.unavailable("login", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || user.Status != "active" {
		return User{}, ErrNotFound
	}
	user.PlatformAdmin = admin == 1
	return user, nil
}

// IssueToken 生成一次性明文令牌，库里只保存摘要。
func (s *Store) IssueToken(ctx context.Context, user User) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, s.unavailable("token", err)
	}
	token := hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().UTC().Add(tokenTTL)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_token (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hex.EncodeToString(sum[:]), user.ID, expires)
	if err != nil {
		return "", time.Time{}, s.unavailable("token", err)
	}
	return token, expires, nil
}

// UserByToken 从 Authorization 摘要找回用户。过期和停用都视为未登录。
func (s *Store) UserByToken(ctx context.Context, token string) (User, error) {
	sum := sha256.Sum256([]byte(token))
	var user User
	var admin int
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.tenant_id, u.username, u.platform_admin, u.status
		FROM api_token t JOIN app_user u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.expires_at > UTC_TIMESTAMP(3)`, hex.EncodeToString(sum[:])).Scan(
		&user.ID, &user.TenantID, &user.Username, &admin, &user.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, s.unavailable("token_lookup", err)
	}
	if user.Status != "active" {
		return User{}, ErrNotFound
	}
	user.PlatformAdmin = admin == 1
	return user, nil
}

var dummyHash []byte

func init() {
	hash, err := bcrypt.GenerateFromPassword([]byte("dummy-not-a-login"), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	dummyHash = hash
}

func randomID(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

// HashToken 给调用方保留摘要算法。明文不记录。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
