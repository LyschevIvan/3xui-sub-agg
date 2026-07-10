package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

// inboundEdit parses semantic identifiers and delegates all fresh-document,
// lossless mutation, and generation fencing to the aggregator.
func (h *Handler) inboundEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	serverID, serverErr := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, inboundErr := strconv.Atoi(r.FormValue("inbound_id"))
	port, portErr := strconv.Atoi(r.FormValue("new_port"))
	if serverErr != nil || serverID <= 0 || inboundErr != nil || inboundID <= 0 ||
		portErr != nil || port <= 0 || port > 65535 {
		http.Error(w, "bad request: server_id, inbound_id, new_port обязательны", http.StatusBadRequest)
		return
	}
	back := serverInboundsURL(serverID)
	err := h.Agg.EditInbound(r.Context(), user.ID, serverID, inboundID, aggregator.InboundPatch{
		Remark: r.FormValue("new_remark"), Port: port, Enable: r.FormValue("enable") == "1",
	})
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось обновить inbound. Проверьте состояние панели и повторите попытку.", back)
		return
	}
	h.setFlash(w, flashSuccess, "Inbound обновлён")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (h *Handler) inboundCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	sourceServerID, sourceServerErr := strconv.ParseInt(r.FormValue("source_server_id"), 10, 64)
	sourceInboundID, sourceInboundErr := strconv.Atoi(r.FormValue("source_inbound_id"))
	targetServerID, targetServerErr := strconv.ParseInt(r.FormValue("target_server_id"), 10, 64)
	port, portErr := strconv.Atoi(r.FormValue("new_port"))
	if sourceServerErr != nil || sourceServerID <= 0 || sourceInboundErr != nil || sourceInboundID <= 0 ||
		targetServerErr != nil || targetServerID <= 0 || portErr != nil || port <= 0 || port > 65535 {
		http.Error(w, "bad request: source_server_id, source_inbound_id, target_server_id, new_port обязательны", http.StatusBadRequest)
		return
	}
	back := serverInboundsURL(sourceServerID)
	_, err := h.Agg.CopyInbound(r.Context(), aggregator.CopyInboundRequest{
		UserID: user.ID, SourceServerID: sourceServerID, SourceInboundID: sourceInboundID,
		TargetServerID: targetServerID, Remark: r.FormValue("new_remark"), Port: port,
	})
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось скопировать inbound. Проверьте параметры и состояние панелей.", back)
		return
	}
	h.setFlash(w, flashSuccess, "Inbound скопирован")
	http.Redirect(w, r, serverInboundsURL(targetServerID), http.StatusSeeOther)
}

func (h *Handler) inboundDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.FromContext(r.Context())
	serverID, serverErr := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, inboundErr := strconv.Atoi(r.FormValue("inbound_id"))
	if serverErr != nil || serverID <= 0 || inboundErr != nil || inboundID <= 0 {
		http.Error(w, "bad request: server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}
	back := serverInboundsURL(serverID)
	if err := h.Agg.DeleteInbound(r.Context(), user.ID, serverID, inboundID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Не удалось удалить inbound. Проверьте состояние панели и повторите попытку.", back)
		return
	}
	h.setFlash(w, flashSuccess, "Inbound и его подключения удалены; общие записи клиентов сохранены в 3x-ui")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func serverInboundsURL(serverID int64) string {
	return fmt.Sprintf("/dashboard/servers/%d#inbounds", serverID)
}
