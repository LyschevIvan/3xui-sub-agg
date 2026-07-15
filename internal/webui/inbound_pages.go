package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
)

type inboundsPageData struct {
	Inbounds []catalogInbound
	Query    string
	Protocol string
	ServerID int64
}

type inboundDetailData struct {
	Inbound       catalogInbound
	Subscriptions []catalogSubscription
}

func (h *Handler) inboundsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	catalog, err := h.loadCatalog(r, user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	protocol := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("protocol")))
	serverID, _ := strconv.ParseInt(r.URL.Query().Get("server_id"), 10, 64)
	filtered := make([]catalogInbound, 0, len(catalog.Inbounds))
	for _, inbound := range catalog.Inbounds {
		if serverID > 0 && inbound.ServerID != serverID {
			continue
		}
		if protocol != "" && inbound.Protocol != protocol {
			continue
		}
		haystack := strings.ToLower(inbound.ServerName + " " + inbound.Remark + " " + inbound.Endpoint)
		if query != "" && !strings.Contains(haystack, query) {
			continue
		}
		filtered = append(filtered, inbound)
	}
	h.render(w, r, "inbounds.html", &pageData{
		Title: "Inbound'ы", Section: "inbounds",
		InboundsPage: &inboundsPageData{Inbounds: filtered, Query: r.URL.Query().Get("q"), Protocol: protocol, ServerID: serverID},
	})
}

func (h *Handler) inboundDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serverID, serverErr := strconv.ParseInt(r.URL.Query().Get("server_id"), 10, 64)
	inboundID, inboundErr := strconv.Atoi(r.URL.Query().Get("inbound_id"))
	if serverErr != nil || serverID <= 0 || inboundErr != nil || inboundID <= 0 {
		http.NotFound(w, r)
		return
	}
	user := auth.FromContext(r.Context())
	catalog, err := h.loadCatalog(r, user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	inbound, ok := catalog.inbound(serverID, inboundID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	subscriptions := make([]catalogSubscription, 0, len(inbound.SubIDs))
	for _, subID := range inbound.SubIDs {
		if subscription, found := catalog.subscription(subID); found {
			subscriptions = append(subscriptions, subscription)
		}
	}
	h.render(w, r, "inbound_detail.html", &pageData{
		Title: inbound.Remark, Section: "inbounds",
		InboundPage: &inboundDetailData{Inbound: inbound, Subscriptions: subscriptions},
	})
}
