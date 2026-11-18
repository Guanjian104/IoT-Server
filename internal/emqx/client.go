// Package emqx 调用本机 Dashboard API 查询和断开客户端。
package emqx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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

// ConnectedDevices 返回当前仍连接的客户端及其连接时间（毫秒）。
// 第二个结果表示分页已经扫完。调用失败时不返回残缺列表。
func (c *Client) ConnectedDevices(ctx context.Context) (map[string]int64, bool, error) {
	if c == nil || c.baseURL == "" {
		return nil, false, errors.New("clients failed")
	}
	token, err := c.bearer(ctx, false)
	if err != nil {
		return nil, false, errors.New("clients failed")
	}
	online := map[string]int64{}
	for page := 1; page <= 100; page++ {
		status, rows, hasNext, err := c.clientsPage(ctx, token, page)
		if err != nil {
			return nil, false, errors.New("clients failed")
		}
		if status == http.StatusUnauthorized && page == 1 {
			token, err = c.bearer(ctx, true)
			if err != nil {
				return nil, false, errors.New("clients failed")
			}
			status, rows, hasNext, err = c.clientsPage(ctx, token, page)
			if err != nil {
				return nil, false, errors.New("clients failed")
			}
		}
		if status != http.StatusOK {
			return nil, false, errors.New("clients failed")
		}
		for _, row := range rows {
			if row.clientID == "" || !row.connected {
				continue
			}
			online[row.clientID] = row.connectedAt
		}
		if !hasNext {
			return online, true, nil
		}
	}
	return online, false, nil
}

type clientRow struct {
	clientID    string
	connected   bool
	connectedAt int64
}

func (c *Client) clientsPage(ctx context.Context, token string, page int) (int, []clientRow, bool, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/v5/clients")
	if err != nil {
		return 0, nil, false, err
	}
	query := endpoint.Query()
	query.Set("page", strconv.Itoa(page))
	query.Set("limit", "100")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, false, err
	}
	var pageBody struct {
		Data []struct {
			ClientID    string          `json:"clientid"`
			Connected   bool            `json:"connected"`
			ConnState   string          `json:"conn_state"`
			ConnectedAt json.RawMessage `json:"connected_at"`
		} `json:"data"`
		Meta *struct {
			HasNext bool `json:"hasnext"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &pageBody); err != nil || pageBody.Meta == nil {
		return 0, nil, false, errors.New("clients failed")
	}
	rows := make([]clientRow, 0, len(pageBody.Data))
	for _, item := range pageBody.Data {
		rows = append(rows, clientRow{
			clientID:    item.ClientID,
			connected:   item.Connected || item.ConnState == "connected",
			connectedAt: connectedMillis(item.ConnectedAt),
		})
	}
	return resp.StatusCode, rows, pageBody.Meta.HasNext, nil
}

func connectedMillis(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0
		}
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return scaleUnix(n)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UnixMilli()
		}
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return 0
		}
		n = int64(f)
	}
	return scaleUnix(n)
}

func scaleUnix(n int64) int64 {
	if n > 0 && n < 1_000_000_000_000 {
		return n * 1000
	}
	return n
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
