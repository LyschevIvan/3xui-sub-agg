package webui

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type onboardingInbound struct {
	ServerID   int64
	ServerName string
	ID         int
	Remark     string
	Port       int
	Network    string
	Security   string
}

type serverOnboardingData struct {
	Server         storage.Server
	TargetInbounds []onboardingInbound
	SourceInbounds []onboardingInbound
	Groups         []storage.ClientGroup
	TotalClients   int
}

func serverOnboardingURL(serverID int64) string {
	return fmt.Sprintf("/dashboard/onboarding/%d", serverID)
}

func (h *Handler) serverOnboarding(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/dashboard/onboarding/"), "/"), "/")
	if len(parts) < 1 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	serverID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || serverID <= 0 {
		http.NotFound(w, r)
		return
	}
	user := auth.FromContext(r.Context())
	server, err := h.Store.ServerByID(user.ID, serverID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.renderServerOnboarding(w, r, *server)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch parts[1] {
	case "complete":
		if err := h.Store.CompleteServerOnboarding(user.ID, serverID); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		h.setFlash(w, flashSuccess, "Настройка сервера завершена")
		http.Redirect(w, r, "/dashboard/servers/"+strconv.FormatInt(serverID, 10), http.StatusSeeOther)
	case "copy-inbound":
		h.onboardingCopyInbound(w, r, user.ID, serverID)
	case "copy-users":
		h.onboardingCopyUsers(w, r, user.ID, serverID)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) renderServerOnboarding(w http.ResponseWriter, r *http.Request, server storage.Server) {
	user := auth.FromContext(r.Context())
	data := &serverOnboardingData{Server: server}
	for _, snapServer := range h.Agg.Snapshot().Servers {
		if snapServer.UserID != user.ID {
			continue
		}
		for _, inbound := range snapServer.Inbounds {
			if !inbound.Enable || !strings.EqualFold(inbound.Protocol, "vless") {
				continue
			}
			item := onboardingInbound{ServerID: snapServer.ID, ServerName: snapServer.Name, ID: inbound.ID, Remark: inbound.Remark, Port: inbound.Port, Network: networkLabel(inbound.Network), Security: securityLabel(inbound.Security)}
			if snapServer.ID == server.ID {
				data.TargetInbounds = append(data.TargetInbounds, item)
			} else {
				data.SourceInbounds = append(data.SourceInbounds, item)
			}
		}
	}
	data.Groups, _ = h.Store.ListClientGroups(user.ID)
	data.TotalClients = len(buildClientCards(h.Agg.Snapshot(), user.ID, h.subscriptionBase(r, user)))
	h.render(w, r, "server_onboarding.html", &pageData{Title: "Настройка сервера", Section: "servers", Onboarding: data})
}

func (h *Handler) onboardingCopyInbound(w http.ResponseWriter, r *http.Request, userID, targetServerID int64) {
	sourceParts := strings.SplitN(r.FormValue("source"), ":", 2)
	if len(sourceParts) != 2 {
		h.flashErrAndRedirect(w, r, "Выберите inbound-источник", serverOnboardingURL(targetServerID))
		return
	}
	sourceServerID, serverErr := strconv.ParseInt(sourceParts[0], 10, 64)
	sourceInboundID, inboundErr := strconv.Atoi(sourceParts[1])
	port, portErr := strconv.Atoi(r.FormValue("new_port"))
	if serverErr != nil || inboundErr != nil || portErr != nil {
		h.flashErrAndRedirect(w, r, "Проверьте источник и порт", serverOnboardingURL(targetServerID))
		return
	}
	_, err := h.Agg.CopyInbound(r.Context(), aggregator.CopyInboundRequest{UserID: userID, SourceServerID: sourceServerID, SourceInboundID: sourceInboundID, TargetServerID: targetServerID, Remark: r.FormValue("new_remark"), Port: port})
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось скопировать inbound. Проверьте параметры и состояние панелей.", serverOnboardingURL(targetServerID))
		return
	}
	h.setFlash(w, flashSuccess, "Inbound скопирован. Теперь выберите пользователей")
	http.Redirect(w, r, serverOnboardingURL(targetServerID), http.StatusSeeOther)
}

func (h *Handler) onboardingCopyUsers(w http.ResponseWriter, r *http.Request, userID, targetServerID int64) {
	inboundID, err := strconv.Atoi(r.FormValue("target_inbound_id"))
	if err != nil || inboundID <= 0 {
		h.flashErrAndRedirect(w, r, "Выберите целевой inbound", serverOnboardingURL(targetServerID))
		return
	}
	cards := buildClientCards(h.Agg.Snapshot(), userID, "")
	discoverable := make(map[string]struct{}, len(cards))
	for _, card := range cards {
		discoverable[card.Sub] = struct{}{}
	}
	selected := make(map[string]struct{})
	if r.FormValue("scope") == "groups" {
		for _, rawID := range r.Form["group_id"] {
			groupID, parseErr := strconv.ParseInt(rawID, 10, 64)
			if parseErr != nil {
				continue
			}
			group, groupErr := h.Store.ClientGroupByID(userID, groupID)
			if groupErr != nil {
				continue
			}
			for _, subID := range group.Members {
				if _, ok := discoverable[subID]; ok {
					selected[subID] = struct{}{}
				}
			}
		}
	} else {
		selected = discoverable
	}
	subIDs := make([]string, 0, len(selected))
	for subID := range selected {
		subIDs = append(subIDs, subID)
	}
	sort.Strings(subIDs)
	if len(subIDs) == 0 {
		h.flashErrAndRedirect(w, r, "В выбранном наборе нет доступных пользователей", serverOnboardingURL(targetServerID))
		return
	}
	result, copyErr := h.Agg.CopyGroupsToInbound(r.Context(), userID, targetServerID, inboundID, subIDs)
	if copyErr != nil {
		h.flashErrAndRedirect(w, r, "Не удалось запустить добавление пользователей", serverOnboardingURL(targetServerID))
		return
	}
	message := fmt.Sprintf("Добавлено: %d · уже были подключены: %d · не удалось: %d", result.Added, result.AlreadyAttached, result.Failed)
	if result.Failed > 0 {
		h.flashErrAndRedirect(w, r, message+". Повторите операцию для оставшихся пользователей.", serverOnboardingURL(targetServerID))
		return
	}
	_ = h.Store.CompleteServerOnboarding(userID, targetServerID)
	h.setFlash(w, flashSuccess, message)
	http.Redirect(w, r, "/dashboard/servers/"+strconv.FormatInt(targetServerID, 10), http.StatusSeeOther)
}
