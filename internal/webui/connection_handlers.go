package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type connectionScope string

const (
	connectionScopeAll           connectionScope = "all"
	connectionScopeGroups        connectionScope = "groups"
	connectionScopeSubscriptions connectionScope = "subscriptions"
)

var errInvalidConnectionSelection = errors.New("invalid connection selection")

type connectionSelection struct {
	Scope           connectionScope
	GroupIDs        []int64
	SubIDs          []string
	TargetServerID  int64
	TargetInboundID int
}

type connectionPreview struct {
	Selection       connectionSelection
	Target          catalogInbound
	Selected        []string
	ToAdd           []string
	AlreadyAttached []string
	Unavailable     []string
}

type connectionPlannerData struct {
	Groups        []storage.ClientGroup
	Inbounds      []catalogInbound
	Subscriptions []catalogSubscription
	Selection     connectionSelection
	Preview       *connectionPreview
}

func inboundDetailURL(serverID int64, inboundID int) string {
	return fmt.Sprintf("/dashboard/inbounds/view?server_id=%d&inbound_id=%d", serverID, inboundID)
}

func normalizeConnectionSubIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (h *Handler) buildConnectionPreview(
	ctx context.Context,
	userID int64,
	selection connectionSelection,
) (connectionPreview, error) {
	if err := ctx.Err(); err != nil {
		return connectionPreview{}, err
	}
	memberships, err := h.Store.ClientGroupMemberships(userID)
	if err != nil {
		return connectionPreview{}, err
	}
	catalog := buildCatalog(h.Agg.Snapshot(), userID, "", memberships)
	target, ok := catalog.inbound(selection.TargetServerID, selection.TargetInboundID)
	if !ok || !target.Mutable {
		return connectionPreview{}, errInvalidConnectionSelection
	}

	var requested []string
	switch selection.Scope {
	case connectionScopeAll:
		for _, subscription := range catalog.Subscriptions {
			requested = append(requested, subscription.SubID)
		}
	case connectionScopeGroups:
		if len(selection.GroupIDs) == 0 {
			return connectionPreview{}, errInvalidConnectionSelection
		}
		for _, groupID := range selection.GroupIDs {
			group, groupErr := h.Store.ClientGroupByID(userID, groupID)
			if groupErr != nil {
				return connectionPreview{}, groupErr
			}
			requested = append(requested, group.Members...)
		}
	case connectionScopeSubscriptions:
		requested = append(requested, selection.SubIDs...)
	default:
		return connectionPreview{}, errInvalidConnectionSelection
	}
	requested = normalizeConnectionSubIDs(requested)
	if len(requested) == 0 || len(requested) > 500 {
		return connectionPreview{}, errInvalidConnectionSelection
	}

	discoverable := make(map[string]struct{}, len(catalog.Subscriptions))
	for _, subscription := range catalog.Subscriptions {
		discoverable[subscription.SubID] = struct{}{}
	}
	attached := make(map[string]struct{}, len(target.SubIDs))
	for _, subID := range target.SubIDs {
		attached[subID] = struct{}{}
	}

	preview := connectionPreview{Selection: selection, Target: target, Selected: requested}
	for _, subID := range requested {
		if _, ok := discoverable[subID]; !ok {
			preview.Unavailable = append(preview.Unavailable, subID)
		} else if _, ok := attached[subID]; ok {
			preview.AlreadyAttached = append(preview.AlreadyAttached, subID)
		} else {
			preview.ToAdd = append(preview.ToAdd, subID)
		}
	}
	return preview, nil
}

func parseConnectionSelection(r *http.Request) connectionSelection {
	selection := connectionSelection{Scope: connectionScope(r.FormValue("scope"))}
	if selection.Scope == "" {
		selection.Scope = connectionScopeAll
	}
	selection.TargetServerID, _ = strconv.ParseInt(r.FormValue("target_server_id"), 10, 64)
	selection.TargetInboundID, _ = strconv.Atoi(r.FormValue("target_inbound_id"))
	selection.SubIDs = normalizeConnectionSubIDs(r.Form["sub_id"])
	for _, raw := range r.Form["group_id"] {
		groupID, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && groupID > 0 {
			selection.GroupIDs = append(selection.GroupIDs, groupID)
		}
	}
	sort.Slice(selection.GroupIDs, func(i, j int) bool { return selection.GroupIDs[i] < selection.GroupIDs[j] })
	return selection
}

func (h *Handler) loadConnectionPlannerData(
	r *http.Request,
	userID int64,
	selection connectionSelection,
) (*connectionPlannerData, error) {
	groups, err := h.Store.ListClientGroups(userID)
	if err != nil {
		return nil, err
	}
	catalog, err := h.loadCatalog(r, userID)
	if err != nil {
		return nil, err
	}
	return &connectionPlannerData{
		Groups: groups, Inbounds: catalog.Inbounds, Subscriptions: catalog.Subscriptions,
		Selection: selection,
	}, nil
}

func (h *Handler) connectionNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := auth.FromContext(r.Context())
	selection := parseConnectionSelection(r)
	data, err := h.loadConnectionPlannerData(r, user.ID, selection)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "connections.html", &pageData{
		Title: "Подключить подписки", Section: "inbounds", Connections: data,
	})
}

func (h *Handler) connectionPreviewPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := auth.FromContext(r.Context())
	selection := parseConnectionSelection(r)
	data, err := h.loadConnectionPlannerData(r, user.ID, selection)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	preview, err := h.buildConnectionPreview(r.Context(), user.ID, selection)
	page := &pageData{Title: "Проверка подключения", Section: "inbounds", Connections: data}
	if err != nil {
		page.Error = "Не удалось построить план. Проверьте аудиторию и доступность inbound."
	} else {
		data.Preview = &preview
	}
	h.render(w, r, "connections.html", page)
}

func (h *Handler) connectionApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := auth.FromContext(r.Context())
	selection := parseConnectionSelection(r)
	preview, err := h.buildConnectionPreview(r.Context(), user.ID, selection)
	if err != nil {
		h.flashErrAndRedirect(w, r, "План подключения устарел. Проверьте его ещё раз.", "/dashboard/connections/new")
		return
	}
	if len(preview.ToAdd) == 0 {
		h.setFlash(w, flashSuccess, fmt.Sprintf("Все %d доступных подписок уже подключены", len(preview.AlreadyAttached)))
		http.Redirect(w, r, inboundDetailURL(preview.Target.ServerID, preview.Target.InboundID), http.StatusSeeOther)
		return
	}
	result, err := h.Agg.CopyGroupsToInbound(
		r.Context(), user.ID, preview.Target.ServerID, preview.Target.InboundID, preview.ToAdd,
	)
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось выполнить подключение. Проверьте состояние сервера.", "/dashboard/connections/new")
		return
	}
	message := fmt.Sprintf("Добавлено: %d · уже подключено: %d · не удалось: %d",
		result.Added, len(preview.AlreadyAttached)+result.AlreadyAttached, result.Failed)
	if result.Failed > 0 {
		h.setFlash(w, flashError, message)
	} else {
		h.setFlash(w, flashSuccess, message)
	}
	http.Redirect(w, r, inboundDetailURL(preview.Target.ServerID, preview.Target.InboundID), http.StatusSeeOther)
}
