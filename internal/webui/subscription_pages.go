package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type subscriptionsPageData struct {
	Subscriptions []catalogSubscription
	Groups        []storage.ClientGroup
	Query         string
	GroupID       int64
}

type subscriptionDetailData struct {
	Subscription catalogSubscription
	AllGroups    []storage.ClientGroup
}

func (h *Handler) subscriptionsPage(w http.ResponseWriter, r *http.Request) {
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
	groups, err := h.Store.ListClientGroups(user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	groupID, _ := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64)
	filtered := make([]catalogSubscription, 0, len(catalog.Subscriptions))
	for _, subscription := range catalog.Subscriptions {
		if query != "" && !strings.Contains(strings.ToLower(subscription.SubID+" "+subscription.Emails), query) {
			continue
		}
		if groupID > 0 && !subscriptionInGroup(subscription, groupID) {
			continue
		}
		filtered = append(filtered, subscription)
	}
	h.render(w, r, "subscriptions.html", &pageData{
		Title: "Подписки", Section: "subscriptions",
		SubscriptionsPage: &subscriptionsPageData{Subscriptions: filtered, Groups: groups, Query: r.URL.Query().Get("q"), GroupID: groupID},
	})
}

func subscriptionInGroup(subscription catalogSubscription, groupID int64) bool {
	for _, group := range subscription.Groups {
		if group.ID == groupID {
			return true
		}
	}
	return false
}

func (h *Handler) subscriptionDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subID := r.URL.Query().Get("sub_id")
	if subID == "" {
		http.NotFound(w, r)
		return
	}
	user := auth.FromContext(r.Context())
	catalog, err := h.loadCatalog(r, user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	subscription, ok := catalog.subscription(subID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	groups, err := h.Store.ListClientGroups(user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "subscription_detail.html", &pageData{
		Title: subID, Section: "subscriptions",
		SubscriptionPage: &subscriptionDetailData{Subscription: subscription, AllGroups: groups},
	})
}
