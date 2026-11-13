// Package emqx 调用本机 Dashboard API 断开客户端。
package emqx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Client 只访问已配置的本机 API。管理接口使用登录后的令牌，不把令牌写入日志。
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	mu       sync.Mutex
	token    string
}

// New 创建踢除客户端。baseURL 形如 http://127.0.0.1:18083。
func New(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     &http.Client{Timeout: 3 * time.Second},
	}
}

// Kick 断开指定客户端。对方已经不在线时视为成功。
func (c *Client) Kick(ctx context.Context, clientID string) error {
	if c == nil || !clientIDPattern.MatchString(clientID) {
		return errors.New("kick failed")
	}
	token, err := c.bearer(ctx, false)
	if err != nil {
		return errors.New("kick failed")
	}
	status, err := c.delete(ctx, clientID, token)
	if err != nil {
		return errors.New("kick failed")
	}
	if status == http.StatusUnauthorized {
		token, err = c.bearer(ctx, true)
		if err != nil {
			return errors.New("kick failed")
		}
		status, err = c.delete(ctx, clientID, token)
		if err != nil {
			return errors.New("kick failed")
		}
	}
	if status == http.StatusNotFound || status == http.StatusNoContent || status == http.StatusOK {
		return nil
	}
	return errors.New("kick failed")
}

func (c *Client) delete(ctx context.Context, clientID, token string) (int, error) {
	endpoint, err := url.JoinPath(c.baseURL, "/api/v5/clients", clientID)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (c *Client) bearer(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && !force {
		return c.token, nil
	}
	payload, err := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v5/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("login failed")
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Token == "" {
		return "", errors.New("login failed")
	}
	c.token = body.Token
	return c.token, nil
}
