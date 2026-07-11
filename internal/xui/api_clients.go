package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type ClientSummary struct {
	RecordID   *int   `json:"id,omitempty"`
	Email      string `json:"email"`
	SubID      string `json:"subId"`
	Enable     bool   `json:"enable"`
	TotalGB    int64  `json:"totalGB"`
	ExpiryTime int64  `json:"expiryTime"`
	LimitIP    int    `json:"limitIp"`
	Reset      int    `json:"reset"`
	Group      string `json:"group,omitempty"`
	Comment    string `json:"comment,omitempty"`
	InboundIDs []int  `json:"inboundIds"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

type ClientPage struct {
	Items    []ClientSummary `json:"items"`
	Total    int             `json:"total"`
	Filtered int             `json:"filtered"`
	Page     int             `json:"page"`
	PageSize int             `json:"pageSize"`
}

type ClientReverse struct {
	Tag string `json:"tag"`
}

type ClientPayload struct {
	ID           string         `json:"id,omitempty"`
	Security     string         `json:"security"`
	Password     string         `json:"password,omitempty"`
	Flow         string         `json:"flow,omitempty"`
	Reverse      *ClientReverse `json:"reverse,omitempty"`
	Auth         string         `json:"auth,omitempty"`
	PrivateKey   string         `json:"privateKey,omitempty"`
	PublicKey    string         `json:"publicKey,omitempty"`
	AllowedIPs   []string       `json:"allowedIPs,omitempty"`
	PreSharedKey string         `json:"preSharedKey,omitempty"`
	KeepAlive    int            `json:"keepAlive,omitempty"`
	Email        string         `json:"email"`
	LimitIP      int            `json:"limitIp"`
	TotalGB      int64          `json:"totalGB"`
	ExpiryTime   int64          `json:"expiryTime"`
	Enable       bool           `json:"enable"`
	TgID         int64          `json:"tgId"`
	SubID        string         `json:"subId"`
	Group        string         `json:"group,omitempty"`
	Comment      string         `json:"comment"`
	Reset        int            `json:"reset"`
	CreatedAt    int64          `json:"created_at,omitempty"`
	UpdatedAt    int64          `json:"updated_at,omitempty"`
}

type ClientRecord struct {
	RecordID     int             `json:"id"`
	Email        string          `json:"email"`
	SubID        string          `json:"subId"`
	UUID         string          `json:"uuid"`
	Password     string          `json:"password"`
	Auth         string          `json:"auth"`
	Flow         string          `json:"flow"`
	Security     string          `json:"security"`
	Reverse      json.RawMessage `json:"reverse"`
	PrivateKey   string          `json:"privateKey"`
	PublicKey    string          `json:"publicKey"`
	AllowedIPs   string          `json:"allowedIPs"`
	PreSharedKey string          `json:"preSharedKey"`
	KeepAlive    int             `json:"keepAlive"`
	LimitIP      int             `json:"limitIp"`
	TotalGB      int64           `json:"totalGB"`
	ExpiryTime   int64           `json:"expiryTime"`
	Enable       bool            `json:"enable"`
	TgID         int64           `json:"tgId"`
	Group        string          `json:"group"`
	Comment      string          `json:"comment"`
	Reset        int             `json:"reset"`
	CreatedAt    int64           `json:"createdAt"`
	UpdatedAt    int64           `json:"updatedAt"`
}

type ClientDetail struct {
	Client        ClientRecord      `json:"client"`
	InboundIDs    []int             `json:"inboundIds"`
	ExternalLinks []json.RawMessage `json:"externalLinks"`
	UsedTraffic   int64             `json:"usedTraffic"`
}

type ClientAPI interface {
	ListClients(context.Context) ([]ClientSummary, error)
	GetClient(context.Context, string) (ClientDetail, error)
	AddClient(context.Context, ClientPayload, []int) error
	UpdateClient(context.Context, string, ClientPayload) error
	DeleteClient(context.Context, string) error
	AttachClient(context.Context, string, []int) error
	DetachClient(context.Context, string, []int) error
	SubLinks(context.Context, string, string) ([]string, error)
}

const (
	clientPageSize   = 200
	maxClientPages   = 1000
	maxClientRecords = clientPageSize * maxClientPages
)

func (r ClientRecord) Payload() (ClientPayload, error) {
	var reverse *ClientReverse
	rawReverse := bytes.TrimSpace(r.Reverse)
	if len(rawReverse) > 0 && !bytes.Equal(rawReverse, []byte("null")) {
		var decoded ClientReverse
		if err := json.Unmarshal(rawReverse, &decoded); err != nil {
			return ClientPayload{}, fmt.Errorf("decode client reverse: %w", err)
		}
		reverse = &decoded
	}

	var allowedIPs []string
	if r.AllowedIPs != "" {
		allowedIPs = strings.Split(r.AllowedIPs, ",")
	}

	return ClientPayload{
		ID:           r.UUID,
		Security:     r.Security,
		Password:     r.Password,
		Flow:         r.Flow,
		Reverse:      reverse,
		Auth:         r.Auth,
		PrivateKey:   r.PrivateKey,
		PublicKey:    r.PublicKey,
		AllowedIPs:   allowedIPs,
		PreSharedKey: r.PreSharedKey,
		KeepAlive:    r.KeepAlive,
		Email:        r.Email,
		LimitIP:      r.LimitIP,
		TotalGB:      r.TotalGB,
		ExpiryTime:   r.ExpiryTime,
		Enable:       r.Enable,
		TgID:         r.TgID,
		SubID:        r.SubID,
		Group:        r.Group,
		Comment:      r.Comment,
		Reset:        r.Reset,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}, nil
}

func (c *APIClient) ListClients(ctx context.Context) ([]ClientSummary, error) {
	var clients []ClientSummary
	seenEmails := make(map[string]struct{})

	for pageNumber := 1; pageNumber <= maxClientPages; pageNumber++ {
		query := url.Values{}
		query.Set("page", strconv.Itoa(pageNumber))
		query.Set("pageSize", strconv.Itoa(clientPageSize))
		page, err := doAPI[ClientPage](
			ctx,
			c.transport,
			http.MethodGet,
			"clients/list/paged?"+query.Encode(),
			nil,
			"",
		)
		if err != nil {
			return nil, sanitizeClientError(err, "list clients")
		}
		if len(page.Items) == 0 {
			break
		}

		hasNewEmail := false
		for _, client := range page.Items {
			if _, exists := seenEmails[client.Email]; !exists {
				hasNewEmail = true
			}
		}
		if !hasNewEmail {
			break
		}
		if len(page.Items) > maxClientRecords-len(clients) {
			return nil, clientPaginationLimitError(ctx)
		}
		clients = append(clients, page.Items...)
		for _, client := range page.Items {
			seenEmails[client.Email] = struct{}{}
		}
		if len(clients) >= page.Filtered {
			break
		}
		if pageNumber == maxClientPages || len(clients) >= maxClientRecords {
			return nil, clientPaginationLimitError(ctx)
		}
	}

	return clients, nil
}

func clientPaginationLimitError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &Error{
			Kind:    ErrorTransport,
			Op:      "list clients",
			Message: "request canceled",
			Err:     err,
		}
	}
	return &Error{
		Kind:    ErrorAPI,
		Op:      "list clients",
		Message: "pagination safety limit exceeded",
	}
}

func (c *APIClient) GetClient(ctx context.Context, email string) (ClientDetail, error) {
	detail, err := doAPI[ClientDetail](
		ctx,
		c.transport,
		http.MethodGet,
		"clients/get/"+url.PathEscape(email),
		nil,
		"",
	)
	if err != nil {
		return ClientDetail{}, sanitizeClientError(err, "get client")
	}
	return detail, nil
}

func (c *APIClient) AddClient(ctx context.Context, client ClientPayload, inboundIDs []int) error {
	body := struct {
		Client     ClientPayload `json:"client"`
		InboundIDs []int         `json:"inboundIds"`
	}{
		Client:     client,
		InboundIDs: inboundIDs,
	}
	_, err := doAPI[struct{}](ctx, c.transport, http.MethodPost, "clients/add", body, "")
	return sanitizeClientError(err, "add client")
}

func (c *APIClient) UpdateClient(ctx context.Context, email string, client ClientPayload) error {
	_, err := doAPI[struct{}](
		ctx,
		c.transport,
		http.MethodPost,
		"clients/update/"+url.PathEscape(email),
		client,
		"",
	)
	return sanitizeClientError(err, "update client")
}

func (c *APIClient) DeleteClient(ctx context.Context, email string) error {
	_, err := doAPI[struct{}](
		ctx,
		c.transport,
		http.MethodPost,
		"clients/del/"+url.PathEscape(email),
		nil,
		"",
	)
	return sanitizeClientError(err, "delete client")
}

func (c *APIClient) AttachClient(ctx context.Context, email string, inboundIDs []int) error {
	return c.changeClientInbounds(ctx, email, "attach", inboundIDs)
}

func (c *APIClient) DetachClient(ctx context.Context, email string, inboundIDs []int) error {
	return c.changeClientInbounds(ctx, email, "detach", inboundIDs)
}

func (c *APIClient) changeClientInbounds(ctx context.Context, email, action string, inboundIDs []int) error {
	body := struct {
		InboundIDs []int `json:"inboundIds"`
	}{InboundIDs: inboundIDs}
	_, err := doAPI[struct{}](
		ctx,
		c.transport,
		http.MethodPost,
		"clients/"+url.PathEscape(email)+"/"+action,
		body,
		"",
	)
	return sanitizeClientError(err, action+" client")
}

func (c *APIClient) SubLinks(ctx context.Context, subID, host string) ([]string, error) {
	links, err := doAPI[[]string](
		ctx,
		c.transport,
		http.MethodGet,
		"clients/subLinks/"+url.PathEscape(subID),
		nil,
		host,
	)
	if err != nil {
		return nil, sanitizeClientError(err, "subscription links")
	}
	return links, nil
}

func sanitizeClientError(err error, operation string) error {
	if err == nil {
		return nil
	}

	var typed *Error
	if !errors.As(err, &typed) {
		return &Error{
			Kind:    ErrorTransport,
			Op:      operation,
			Message: "request failed",
			Err:     err,
		}
	}

	sanitized := *typed
	sanitized.Op = operation
	sanitized.Message = "request failed"
	return &sanitized
}
