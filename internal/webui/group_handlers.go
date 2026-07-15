package webui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type clientListRow struct {
	clientCard
	Groups []storage.ClientGroup
}

type clientsPageData struct {
	Clients []clientListRow
	Groups  []storage.ClientGroup
}

type groupsPageData struct {
	Groups []storage.ClientGroup
}

func (h *Handler) clientsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	groups, err := h.Store.ListClientGroups(user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	memberships, err := h.Store.ClientGroupMemberships(user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	cards := buildClientCards(h.Agg.Snapshot(), user.ID, h.subscriptionBase(r, user))
	rows := make([]clientListRow, 0, len(cards))
	for _, card := range cards {
		rows = append(rows, clientListRow{clientCard: card, Groups: memberships[card.Sub]})
	}
	h.render(w, r, "clients.html", &pageData{
		Title: "Пользователи", Section: "clients",
		ClientsPage: &clientsPageData{Clients: rows, Groups: groups},
	})
}

func (h *Handler) groupsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	groups, err := h.Store.ListClientGroups(user.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "groups.html", &pageData{
		Title: "Группы", Section: "groups", GroupsPage: &groupsPageData{Groups: groups},
	})
}

func (h *Handler) groupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	if _, err := h.Store.CreateClientGroup(user.ID, r.FormValue("name")); err != nil {
		message := "Не удалось создать группу"
		if errors.Is(err, storage.ErrClientGroupExists) {
			message = "Группа с таким названием уже есть"
		} else if errors.Is(err, storage.ErrInvalidClientGroup) {
			message = "Название группы должно содержать от 1 до 64 символов"
		}
		h.flashErrAndRedirect(w, r, message, "/dashboard/groups")
		return
	}
	h.setFlash(w, flashSuccess, "Группа создана")
	http.Redirect(w, r, "/dashboard/groups", http.StatusSeeOther)
}

func (h *Handler) groupAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/dashboard/groups/"), "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	groupID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || groupID <= 0 {
		http.NotFound(w, r)
		return
	}
	user := auth.FromContext(r.Context())
	if _, err := h.Store.ClientGroupByID(user.ID, groupID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	back := "/dashboard/groups"
	var actionErr error
	switch {
	case len(parts) == 2 && parts[1] == "delete":
		actionErr = h.Store.DeleteClientGroup(user.ID, groupID)
	case len(parts) == 2 && parts[1] == "rename":
		actionErr = h.Store.RenameClientGroup(user.ID, groupID, r.FormValue("name"))
	case len(parts) == 3 && parts[1] == "members" && parts[2] == "add":
		actionErr = h.Store.AddClientGroupMembers(user.ID, groupID, r.Form["sub_id"])
		back = "/dashboard/clients"
	case len(parts) == 3 && parts[1] == "members" && parts[2] == "remove":
		actionErr = h.Store.RemoveClientGroupMember(user.ID, groupID, r.FormValue("sub_id"))
	default:
		http.NotFound(w, r)
		return
	}
	if actionErr != nil {
		h.flashErrAndRedirect(w, r, "Не удалось изменить группу", back)
		return
	}
	h.setFlash(w, flashSuccess, "Группа обновлена. Подключения на серверах не изменены")
	http.Redirect(w, r, back, http.StatusSeeOther)
}
