package xui

import "encoding/json"

// Client внутри inbound.settings.
type InboundClient struct {
	ID         string `json:"id"`         // UUID
	Email      string `json:"email"`      // имя клиента (наш идентификатор)
	Flow       string `json:"flow"`       // xtls-rprx-vision или ""
	Enable     bool   `json:"enable"`
	LimitIP    int    `json:"limitIp"`
	TotalGB    int64  `json:"totalGB"`
	ExpiryTime int64  `json:"expiryTime"`
	SubID      string `json:"subId"`
	TgID       any    `json:"tgId"`
	Reset      int    `json:"reset"`
}

type inboundSettings struct {
	Clients []InboundClient `json:"clients"`
}

type RealitySettings struct {
	Show        bool     `json:"show"`
	Xver        int      `json:"xver"`
	Dest        string   `json:"dest"`
	ServerNames []string `json:"serverNames"`
	PrivateKey  string   `json:"privateKey"`
	MinClient   string   `json:"minClient"`
	MaxClient   string   `json:"maxClient"`
	MaxTimediff int      `json:"maxTimediff"`
	ShortIds    []string `json:"shortIds"`
	Settings    struct {
		PublicKey   string `json:"publicKey"`
		Fingerprint string `json:"fingerprint"`
		ServerName  string `json:"serverName"`
		SpiderX     string `json:"spiderX"`
	} `json:"settings"`
}

type TCPSettings struct {
	Header struct {
		Type string `json:"type"`
	} `json:"header"`
}

type WSSettings struct {
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

type GRPCSettings struct {
	ServiceName string `json:"serviceName"`
	MultiMode   bool   `json:"multiMode"`
}

type XHTTPSettings struct {
	Path string `json:"path"`
	Host string `json:"host"`
	Mode string `json:"mode"`
}

type StreamSettings struct {
	Network         string           `json:"network"`  // tcp, ws, grpc, xhttp, ...
	Security        string           `json:"security"` // none, tls, reality
	RealitySettings *RealitySettings `json:"realitySettings,omitempty"`
	TCPSettings     *TCPSettings     `json:"tcpSettings,omitempty"`
	WSSettings      *WSSettings      `json:"wsSettings,omitempty"`
	GRPCSettings    *GRPCSettings    `json:"grpcSettings,omitempty"`
	XHTTPSettings   *XHTTPSettings   `json:"xhttpSettings,omitempty"`
}

// ParseClients вытаскивает список клиентов из inbound.settings.
func ParseClients(settingsJSON string) ([]InboundClient, error) {
	if settingsJSON == "" {
		return nil, nil
	}
	var s inboundSettings
	if err := json.Unmarshal([]byte(settingsJSON), &s); err != nil {
		return nil, err
	}
	return s.Clients, nil
}

// ParseStream парсит inbound.streamSettings.
func ParseStream(streamJSON string) (*StreamSettings, error) {
	if streamJSON == "" {
		return &StreamSettings{Network: "tcp", Security: "none"}, nil
	}
	var s StreamSettings
	if err := json.Unmarshal([]byte(streamJSON), &s); err != nil {
		return nil, err
	}
	return &s, nil
}
