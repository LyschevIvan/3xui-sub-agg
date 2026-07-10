package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

// clientInboundAdd attaches one exact subscription group to a native inbound.
// The browser supplies semantic identifiers only; record selection and client
// creation are owned by the aggregator's fresh native inventory path.
func (h *Handler) clientInboundAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	subID := r.FormValue("sub_id")
	serverID, serverErr := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, inboundErr := strconv.Atoi(r.FormValue("inbound_id"))
	if subID == "" || serverErr != nil || serverID <= 0 || inboundErr != nil || inboundID <= 0 {
		http.Error(w, "bad request: sub_id, server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}

	back := "/dashboard#add-" + subIDSlug(subID)
	result, err := h.Agg.AttachGroup(r.Context(), user.ID, serverID, subID, inboundID)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось добавить клиента. Проверьте состояние панели и повторите попытку.", back)
		return
	}
	if result.Noop {
		h.setFlash(w, flashSuccess, "Клиент уже подключён к inbound'у")
	} else {
		h.setFlash(w, flashSuccess, "Клиент добавлен в inbound")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// clientInboundRemove detaches every exact native record for this group and
// inbound. Partial completion is reported from counts, never from raw errors.
func (h *Handler) clientInboundRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	subID := r.FormValue("sub_id")
	serverID, serverErr := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, inboundErr := strconv.Atoi(r.FormValue("inbound_id"))
	if subID == "" || serverErr != nil || serverID <= 0 || inboundErr != nil || inboundID <= 0 {
		http.Error(w, "bad request: sub_id, server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}

	back := "/dashboard#card-" + subIDSlug(subID)
	result, err := h.Agg.DetachGroup(r.Context(), user.ID, serverID, subID, inboundID)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if result.Attempted > 0 && result.Succeeded > 0 {
			h.flashErrAndRedirect(w, r, fmt.Sprintf(
				"Клиент убран частично: %d из %d записей. Повторите попытку.", result.Succeeded, result.Attempted,
			), back)
			return
		}
		h.flashErrAndRedirect(w, r, "Не удалось убрать клиента. Проверьте состояние панели и повторите попытку.", back)
		return
	}
	if result.Noop {
		h.setFlash(w, flashSuccess, "Клиент уже отсутствует в inbound'е")
	} else {
		h.setFlash(w, flashSuccess, "Клиент убран из inbound'а")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
