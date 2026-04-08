package link

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

// BuildVless собирает ссылку vless://... для клиента в конкретном inbound.
// host — публичный адрес сервера (host_override или hostname из api_url).
// Поддержаны: vless + (reality|tls|none) + (tcp|ws|grpc|xhttp).
func BuildVless(host string, port int, remark string, c xui.InboundClient, ss *xui.StreamSettings) (string, error) {
	if c.ID == "" {
		return "", fmt.Errorf("empty client uuid")
	}
	q := url.Values{}

	network := ss.Network
	if network == "" {
		network = "tcp"
	}
	q.Set("type", network)

	security := ss.Security
	if security == "" {
		security = "none"
	}
	q.Set("security", security)

	// Reality
	if security == "reality" && ss.RealitySettings != nil {
		r := ss.RealitySettings
		q.Set("pbk", r.Settings.PublicKey)
		if fp := r.Settings.Fingerprint; fp != "" {
			q.Set("fp", fp)
		} else {
			q.Set("fp", "chrome")
		}
		sni := r.Settings.ServerName
		if sni == "" && len(r.ServerNames) > 0 {
			sni = r.ServerNames[0]
		}
		if sni != "" {
			q.Set("sni", sni)
		}
		if len(r.ShortIds) > 0 {
			q.Set("sid", r.ShortIds[0])
		}
		if spx := r.Settings.SpiderX; spx != "" {
			q.Set("spx", spx)
		}
	}

	// Транспорт
	switch network {
	case "tcp":
		if ss.TCPSettings != nil && ss.TCPSettings.Header.Type != "" {
			q.Set("headerType", ss.TCPSettings.Header.Type)
		}
	case "ws":
		if ss.WSSettings != nil {
			if p := ss.WSSettings.Path; p != "" {
				q.Set("path", p)
			}
			if h, ok := ss.WSSettings.Headers["Host"]; ok && h != "" {
				q.Set("host", h)
			}
		}
	case "grpc":
		if ss.GRPCSettings != nil {
			if n := ss.GRPCSettings.ServiceName; n != "" {
				q.Set("serviceName", n)
			}
			if ss.GRPCSettings.MultiMode {
				q.Set("mode", "multi")
			}
		}
	case "xhttp":
		if ss.XHTTPSettings != nil {
			if p := ss.XHTTPSettings.Path; p != "" {
				q.Set("path", p)
			}
			if h := ss.XHTTPSettings.Host; h != "" {
				q.Set("host", h)
			}
			if m := ss.XHTTPSettings.Mode; m != "" {
				q.Set("mode", m)
			}
		}
	}

	// flow только для reality+tcp (vision)
	if c.Flow != "" && security == "reality" && network == "tcp" {
		q.Set("flow", c.Flow)
	}

	// IPv6 host в скобках
	h := host
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}

	u := &url.URL{
		Scheme:   "vless",
		User:     url.User(c.ID),
		Host:     fmt.Sprintf("%s:%d", h, port),
		RawQuery: q.Encode(),
		Fragment: remark,
	}
	return u.String(), nil
}
