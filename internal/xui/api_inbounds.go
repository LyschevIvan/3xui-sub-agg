package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

var normalizedInboundFields = [...]string{"settings", "streamSettings", "sniffing"}

var errMissingInboundDocument = errors.New("missing inbound response document")

type InboundDocument map[string]json.RawMessage

func (d *InboundDocument) UnmarshalJSON(data []byte) error {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode inbound document: %w", err)
	}
	if decoded == nil {
		return errors.New("decode inbound document: expected JSON object")
	}
	for _, key := range normalizedInboundFields {
		if raw, exists := decoded[key]; exists {
			decoded[key] = normalizeRawJSON(raw)
		}
	}
	*d = decoded
	return nil
}

func (d InboundDocument) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]json.RawMessage(d))
}

func (d InboundDocument) Clone() InboundDocument {
	if d == nil {
		return nil
	}
	clone := make(InboundDocument, len(d))
	for key, raw := range d {
		clone[key] = append(json.RawMessage(nil), raw...)
	}
	return clone
}

func (d InboundDocument) Int(key string) (int, error) {
	raw, exists := d[key]
	if !exists {
		return 0, missingInboundField(key)
	}
	var value int
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return 0, invalidInboundFieldType(key, "integer")
	}
	return value, nil
}

func (d InboundDocument) String(key string) (string, error) {
	raw, exists := d[key]
	if !exists {
		return "", missingInboundField(key)
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", invalidInboundFieldType(key, "string")
	}
	return value, nil
}

func (d InboundDocument) Bool(key string) (bool, error) {
	raw, exists := d[key]
	if !exists {
		return false, missingInboundField(key)
	}
	var value bool
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return false, invalidInboundFieldType(key, "boolean")
	}
	return value, nil
}

func missingInboundField(key string) error {
	return fmt.Errorf("inbound field %q is missing", key)
}

func invalidInboundFieldType(key, expected string) error {
	return fmt.Errorf("inbound field %q must be a JSON %s", key, expected)
}

func (d InboundDocument) Set(key string, value any) error {
	if d == nil {
		return errors.New("set inbound field: document is nil")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode inbound field %q", key)
	}
	d[key] = raw
	return nil
}

func (d InboundDocument) Delete(key string) {
	delete(d, key)
}

func (d InboundDocument) Clients() ([]ClientPayload, error) {
	settings, err := d.settingsObject(false)
	if err != nil {
		return nil, err
	}
	rawClients, exists := settings["clients"]
	if !exists {
		return nil, missingInboundField("settings.clients")
	}
	var clients []ClientPayload
	if bytes.Equal(bytes.TrimSpace(rawClients), []byte("null")) || json.Unmarshal(rawClients, &clients) != nil {
		return nil, invalidInboundFieldType("settings.clients", "array")
	}
	return clients, nil
}

func (d InboundDocument) SetClients(clients []ClientPayload) error {
	if d == nil {
		return errors.New("set inbound clients: document is nil")
	}
	settings, err := d.settingsObject(true)
	if err != nil {
		return err
	}
	if clients == nil {
		clients = []ClientPayload{}
	}
	rawClients, err := json.Marshal(clients)
	if err != nil {
		return errors.New("encode inbound clients")
	}
	settings["clients"] = rawClients
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return errors.New("encode inbound settings")
	}
	d["settings"] = rawSettings
	return nil
}

func (d InboundDocument) settingsObject(createMissing bool) (map[string]json.RawMessage, error) {
	rawSettings, exists := d["settings"]
	if !exists {
		if createMissing {
			return make(map[string]json.RawMessage), nil
		}
		return nil, missingInboundField("settings")
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(rawSettings, &settings); err != nil || settings == nil {
		return nil, invalidInboundFieldType("settings", "object")
	}
	return settings, nil
}

func normalizeRawJSON(raw json.RawMessage) json.RawMessage {
	preserved := append(json.RawMessage(nil), raw...)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return preserved
	}

	var legacy string
	if err := json.Unmarshal(trimmed, &legacy); err != nil {
		return preserved
	}
	nested := []byte(legacy)
	if !json.Valid(nested) {
		return preserved
	}
	return append(json.RawMessage(nil), nested...)
}

type InboundSummary struct {
	ID             int             `json:"id"`
	Remark         string          `json:"remark"`
	Enable         bool            `json:"enable"`
	Port           int             `json:"port"`
	Protocol       string          `json:"protocol"`
	StreamSettings json.RawMessage `json:"streamSettings"`
}

func (i InboundSummary) NetworkSecurity() (network, security string) {
	var stream struct {
		Network  string `json:"network"`
		Security string `json:"security"`
	}
	_ = json.Unmarshal(normalizeRawJSON(i.StreamSettings), &stream)
	if stream.Network == "" {
		stream.Network = "tcp"
	}
	if stream.Security == "" {
		stream.Security = "none"
	}
	return stream.Network, stream.Security
}

type InboundAPI interface {
	ListSlimInbounds(context.Context) ([]InboundSummary, error)
	GetInbound(context.Context, int) (InboundDocument, error)
	AddInbound(context.Context, InboundDocument) (InboundDocument, error)
	UpdateInbound(context.Context, int, InboundDocument) (InboundDocument, error)
	DeleteInbound(context.Context, int) error
}

type PanelAPI interface {
	ServerAPI
	ClientAPI
	InboundAPI
}

func (c *APIClient) ListSlimInbounds(ctx context.Context) ([]InboundSummary, error) {
	inbounds, err := doAPI[[]InboundSummary](
		ctx,
		c.transport,
		http.MethodGet,
		"inbounds/list/slim",
		nil,
		"",
	)
	if err != nil {
		return nil, sanitizeInboundError(err, "list inbounds")
	}
	return inbounds, nil
}

func (c *APIClient) GetInbound(ctx context.Context, id int) (InboundDocument, error) {
	rawDocument, err := doAPI[json.RawMessage](
		ctx,
		c.transport,
		http.MethodGet,
		"inbounds/get/"+strconv.Itoa(id),
		nil,
		"",
	)
	if err != nil {
		return nil, sanitizeInboundError(err, "get inbound")
	}
	return decodeInboundDocument(rawDocument, "get inbound")
}

func (c *APIClient) AddInbound(ctx context.Context, document InboundDocument) (InboundDocument, error) {
	rawCreated, err := doAPI[json.RawMessage](
		ctx,
		c.transport,
		http.MethodPost,
		"inbounds/add",
		document,
		"",
	)
	if err != nil {
		return nil, sanitizeInboundError(err, "add inbound")
	}
	return decodeInboundDocument(rawCreated, "add inbound")
}

func (c *APIClient) UpdateInbound(ctx context.Context, id int, document InboundDocument) (InboundDocument, error) {
	rawUpdated, err := doAPI[json.RawMessage](
		ctx,
		c.transport,
		http.MethodPost,
		"inbounds/update/"+strconv.Itoa(id),
		document,
		"",
	)
	if err != nil {
		return nil, sanitizeInboundError(err, "update inbound")
	}
	return decodeInboundDocument(rawUpdated, "update inbound")
}

func (c *APIClient) DeleteInbound(ctx context.Context, id int) error {
	_, err := doAPI[struct{}](
		ctx,
		c.transport,
		http.MethodPost,
		"inbounds/del/"+strconv.Itoa(id),
		nil,
		"",
	)
	return sanitizeInboundError(err, "delete inbound")
}

func sanitizeInboundError(err error, operation string) error {
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

func decodeInboundDocument(raw json.RawMessage, operation string) (InboundDocument, error) {
	if len(raw) == 0 {
		return nil, sanitizeInboundError(&Error{
			Kind:       ErrorDecode,
			StatusCode: http.StatusOK,
			Err:        errMissingInboundDocument,
		}, operation)
	}

	var document InboundDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, sanitizeInboundError(&Error{
			Kind:       ErrorDecode,
			StatusCode: http.StatusOK,
			Err:        err,
		}, operation)
	}
	return document, nil
}
