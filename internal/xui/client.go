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

// VisionFlow возвращает значение поля flow для VLESS-клиента в зависимости от
// настроек inbound'а. XTLS Vision применим только к Reality+TCP; для остальных
// комбинаций (Reality+WS/gRPC/xHTTP, TLS, none) — пустая строка.
func VisionFlow(network, security string) string {
	n := strings.ToLower(strings.TrimSpace(network))
	if n == "" {
		n = "tcp"
	}
	s := strings.ToLower(strings.TrimSpace(security))
	if n == "tcp" && s == "reality" {
		return "xtls-rprx-vision"
	}
	return ""
}

// AddInbound создаёт новый inbound на сервере. Поле ib.ID игнорируется
// (3x-ui сам назначает id новому inbound'у). Поля settings/streamSettings/
// sniffing должны уже быть JSON-строками — как они приходят из ListInbounds,
// что делает endpoint удобным для копирования inbound'а с другого сервера.
func (c *Client) AddInbound(ctx context.Context, ib RawInbound) error {
	ib.ID = 0
	return c.doMutation(ctx, "panel/api/inbounds/add", ib)
}

// UpdateInbound обновляет inbound целиком (3x-ui ожидает полный объект).
// Получите текущий через ListInbounds, измените нужные поля, передайте сюда.
func (c *Client) UpdateInbound(ctx context.Context, id int, ib RawInbound) error {
	ib.ID = id
	rel := fmt.Sprintf("panel/api/inbounds/update/%d", id)
	return c.doMutation(ctx, rel, ib)
}

// DeleteInbound удаляет inbound вместе со всеми клиентами в нём.
func (c *Client) DeleteInbound(ctx context.Context, id int) error {
	rel := fmt.Sprintf("panel/api/inbounds/del/%d", id)
	return c.doMutation(ctx, rel, nil)
}

// AddClient добавляет клиента в указанный inbound.
// Email должен быть уникален в пределах inbound'а; flow выставляется снаружи
// в зависимости от network/security inbound'а (см. VisionFlow).
func (c *Client) AddClient(ctx context.Context, inboundID int, client InboundClient) error {
	settings, err := encodeClientSettings(client)
	if err != nil {
		return err
	}
	body := map[string]any{"id": inboundID, "settings": settings}
	return c.doMutation(ctx, "panel/api/inbounds/addClient", body)
}

// DeleteClient удаляет клиента по UUID из указанного inbound'а.
func (c *Client) DeleteClient(ctx context.Context, inboundID int, clientUUID string) error {
	rel := fmt.Sprintf("panel/api/inbounds/%d/delClient/%s", inboundID, url.PathEscape(clientUUID))
	return c.doMutation(ctx, rel, nil)
}

// UpdateClient обновляет существующего клиента по UUID.
func (c *Client) UpdateClient(ctx context.Context, inboundID int, client InboundClient) error {
	if client.ID == "" {
		return fmt.Errorf("update client: empty uuid")
	}
	settings, err := encodeClientSettings(client)
	if err != nil {
		return err
	}
	body := map[string]any{"id": inboundID, "settings": settings}
	rel := fmt.Sprintf("panel/api/inbounds/updateClient/%s", url.PathEscape(client.ID))
	return c.doMutation(ctx, rel, body)
}

// encodeClientSettings сериализует одного клиента в JSON-строку, как ожидает
// 3x-ui в поле "settings": `{"clients":[{...}]}`.
func encodeClientSettings(client InboundClient) (string, error) {
	if client.TgID == nil {
		client.TgID = ""
	}
	b, err := json.Marshal(inboundSettings{Clients: []InboundClient{client}})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// doMutation выполняет POST с JSON-телом (или без тела) на mutation-эндпоинт
// 3x-ui, повторяя запрос после релогина при протухшей сессии. Парсит общий
// ответ {success,msg,obj} и возвращает ошибку при success=false.
func (c *Client) doMutation(ctx context.Context, rel string, body any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.auth {
		if err := c.login(ctx); err != nil {
			return err
		}
	}

	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	do := func() (*http.Response, error) {
		var rd io.Reader
		if raw != nil {
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(rel), rd)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		return c.http.Do(req)
	}

	resp, err := do()
	if err != nil {
		return err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTemporaryRedirect || looksLikeLoginPage(respBody) {
		c.auth = false
		if err := c.login(ctx); err != nil {
			return err
		}
		resp, err = do()
		if err != nil {
			return err
		}
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: http %d: %s", rel, resp.StatusCode, truncate(respBody, 200))
	}

	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return fmt.Errorf("POST %s: decode: %w (body=%s)", rel, err, truncate(respBody, 200))
	}
	if !r.Success {
		msg := strings.TrimSpace(r.Msg)
		if msg == "" {
			msg = "(no message)"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
