package xui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client — минимальный клиент к админке 3x-ui (MHSanaei fork).
type Client struct {
	baseURL  string // api_url, без завершающего /
	path     string // webBasePath, всегда вида /xxx/
	username string
	password string

	http *http.Client
	mu   sync.Mutex
	auth bool
}

func New(apiURL, path, username, password string, insecure bool, timeout time.Duration) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	return &Client{
		baseURL:  strings.TrimRight(apiURL, "/"),
		path:     path,
		username: username,
		password: password,
		http: &http.Client{
			Jar:       jar,
			Transport: tr,
			Timeout:   timeout,
		},
	}, nil
}

func (c *Client) url(rel string) string {
	return c.baseURL + c.path + strings.TrimLeft(rel, "/")
}

func (c *Client) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("login"), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login: http %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("login: decode: %w (body=%s)", err, string(body))
	}
	if !r.Success {
		return fmt.Errorf("login failed: %s", r.Msg)
	}
	c.auth = true
	return nil
}

// RawInbound — как его отдаёт 3x-ui. settings / streamSettings приходят как JSON-строки.
type RawInbound struct {
	ID             int    `json:"id"`
	Up             int64  `json:"up"`
	Down           int64  `json:"down"`
	Total          int64  `json:"total"`
	Remark         string `json:"remark"`
	Enable         bool   `json:"enable"`
	ExpiryTime     int64  `json:"expiryTime"`
	Listen         string `json:"listen"`
	Port           int    `json:"port"`
	Protocol       string `json:"protocol"`
	Settings       string `json:"settings"`
	StreamSettings string `json:"streamSettings"`
	Tag            string `json:"tag"`
	Sniffing       string `json:"sniffing"`
}

type inboundsResp struct {
	Success bool         `json:"success"`
	Msg     string       `json:"msg"`
	Obj     []RawInbound `json:"obj"`
}

// ListInbounds возвращает все inbound'ы с сервера. При необходимости логинится заново.
func (c *Client) ListInbounds(ctx context.Context) ([]RawInbound, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.auth {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}

	// Разные форки 3x-ui расходятся: где-то POST, где-то GET. Пробуем GET, при 404 — POST.
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("panel/api/inbounds/list"), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	resp, err := do()
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// если сессия протухла — перелогин и повтор
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTemporaryRedirect || looksLikeLoginPage(body) {
		c.auth = false
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		resp, err = do()
		if err != nil {
			return nil, err
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list inbounds: http %d: %s", resp.StatusCode, truncate(body, 200))
	}

	var r inboundsResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("list inbounds: decode: %w (body=%s)", err, truncate(body, 200))
	}
	if !r.Success {
		return nil, fmt.Errorf("list inbounds: %s", r.Msg)
	}
	return r.Obj, nil
}

func looksLikeLoginPage(body []byte) bool {
	return bytes.Contains(body, []byte("<html")) || bytes.Contains(body, []byte("loginForm"))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
