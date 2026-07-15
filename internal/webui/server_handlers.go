package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type serverConnectionChecker interface {
	Check(context.Context, storage.Server, string) (xui.ServerStatus, error)
}

type xuiConnectionChecker struct {
	timeout time.Duration
}

func (c xuiConnectionChecker) Check(ctx context.Context, srv storage.Server, token string) (xui.ServerStatus, error) {
	client, err := xui.NewAPI(xui.APIConfig{
		BaseURL:            srv.APIURL,
		PanelPath:          srv.Path,
		Token:              token,
		InsecureSkipVerify: srv.InsecureSkipVerify,
		Timeout:            c.timeout,
	})
	if err != nil {
		return xui.ServerStatus{}, err
	}
	return client.Validate(ctx)
}

type serverFormData struct {
	ID                  int64
	Name                string
	APIURL              string
	Path                string
	HostOverride        string
	InsecureSkipVerify  bool
	HasAPIToken         bool
	CanStoreAPIToken    bool
	UsesHTTP            bool
	PanelVersion        string
	OnboardingCompleted bool
	Inbounds            []serverEditInbound
	OtherServers        []serverOption
	InboundsErr         string
}

type serverEditInbound struct {
	ID         int
	Remark     string
	Port       int
	Network    string
	Security   string
	SecurityCl string
	Enable     bool
}

type serverOption struct {
	ID   int64
	Name string
}

func parseServerForm(r *http.Request) (serverFormData, string, error) {
	f := serverFormData{
		Name:               strings.TrimSpace(r.FormValue("name")),
		APIURL:             strings.TrimSpace(r.FormValue("api_url")),
		Path:               strings.TrimSpace(r.FormValue("path")),
		HostOverride:       strings.TrimSpace(r.FormValue("host_override")),
		InsecureSkipVerify: r.FormValue("insecure_skip_verify") == "1",
	}
	token := r.FormValue("api_token")
	if f.Name == "" || f.APIURL == "" {
		return f, token, errors.New("name и api_url обязательны")
	}
	parsed, err := url.Parse(f.APIURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return f, token, errors.New("api_url должен быть корректным HTTP(S) URL с хостом")
	}
	f.UsesHTTP = parsed.Scheme == "http"
	if f.Path == "" {
		f.Path = "/"
	}
	if !strings.HasPrefix(f.Path, "/") {
		f.Path = "/" + f.Path
	}
	if !strings.HasSuffix(f.Path, "/") {
		f.Path += "/"
	}
	return f, token, nil
}

type connectionCheckResponse struct {
	OK           bool   `json:"ok"`
	PanelVersion string `json:"panel_version,omitempty"`
	Code         string `json:"code,omitempty"`
	Message      string `json:"message,omitempty"`
}

func (h *Handler) serverCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeConnectionCheck(w, http.StatusMethodNotAllowed, connectionCheckResponse{
			Code: "method_not_allowed", Message: "Метод не поддерживается",
		})
		return
	}

	user := auth.FromContext(r.Context())
	var stored *storage.Server
	if rawID := strings.TrimSpace(r.FormValue("server_id")); rawID != "" {
		serverID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || serverID <= 0 {
			writeConnectionCheck(w, http.StatusBadRequest, connectionCheckResponse{
				Code: "invalid_form", Message: "Некорректный идентификатор сервера",
			})
			return
		}
		stored, err = h.Store.ServerByID(user.ID, serverID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				writeConnectionCheck(w, http.StatusNotFound, connectionCheckResponse{
					Code: "not_found", Message: "Сервер не найден",
				})
				return
			}
			writeConnectionCheck(w, http.StatusInternalServerError, connectionCheckResponse{
				Code: "storage", Message: "Не удалось прочитать настройки сервера",
			})
			return
		}
	}

	form, token, err := parseServerForm(r)
	if err != nil {
		writeConnectionCheck(w, http.StatusBadRequest, connectionCheckResponse{
			Code: "invalid_form", Message: err.Error(),
		})
		return
	}
	candidate := storage.Server{
		UserID: user.ID, Name: form.Name, APIURL: form.APIURL, Path: form.Path,
		HostOverride: form.HostOverride, InsecureSkipVerify: form.InsecureSkipVerify,
	}
	if stored != nil {
		candidate.ID = stored.ID
		if token == "" {
			if stored.TokenError != nil || !stored.HasAPIToken || stored.APIToken == "" {
				writeConnectionCheck(w, http.StatusConflict, savedTokenConfigurationError())
				return
			}
			token = stored.APIToken
		}
	} else if token == "" {
		writeConnectionCheck(w, http.StatusBadRequest, connectionCheckResponse{
			Code: "token_required", Message: "API-токен обязателен",
		})
		return
	}

	status, err := h.checker.Check(r.Context(), candidate, token)
	if err != nil {
		response := mapConnectionError(err)
		writeConnectionCheck(w, http.StatusBadGateway, response)
		return
	}
	if status.PanelVersion == "" || strings.Contains(status.PanelVersion, token) {
		writeConnectionCheck(w, http.StatusBadGateway, connectionCheckResponse{
			Code: "decode", Message: "Панель вернула некорректный ответ",
		})
		return
	}
	writeConnectionCheck(w, http.StatusOK, connectionCheckResponse{
		OK: true, PanelVersion: status.PanelVersion,
	})
}

func (h *Handler) serverNew(w http.ResponseWriter, r *http.Request) {
	data := &pageData{
		Title: "Новый сервер", Section: "servers",
		Form: serverFormData{Path: "/", CanStoreAPIToken: h.Store.CanStoreAPITokens()},
	}
	if r.Method == http.MethodGet {
		h.render(w, r, "server_form.html", data)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	form, token, err := parseServerForm(r)
	form.CanStoreAPIToken = h.Store.CanStoreAPITokens()
	data.Form = form
	if err != nil {
		h.renderServerFormError(w, r, data, http.StatusBadRequest, err.Error())
		return
	}
	if token == "" {
		h.renderServerFormError(w, r, data, http.StatusBadRequest, "API-токен обязателен")
		return
	}

	user := auth.FromContext(r.Context())
	candidate := storage.Server{
		UserID: user.ID, Name: form.Name, APIURL: form.APIURL, Path: form.Path,
		HostOverride: form.HostOverride, InsecureSkipVerify: form.InsecureSkipVerify,
	}
	if _, err := h.checker.Check(r.Context(), candidate, token); err != nil {
		h.renderServerFormError(w, r, data, http.StatusBadGateway, mapConnectionError(err).Message)
		return
	}
	candidate.APIToken = token
	created, err := h.Store.CreateServer(&candidate)
	if err != nil {
		status, message := mapStorageSaveError(err)
		h.renderServerFormError(w, r, data, status, message)
		return
	}
	h.Agg.Trigger()
	http.Redirect(w, r, serverOnboardingURL(created.ID), http.StatusSeeOther)
}

func (h *Handler) serverEdit(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dashboard/servers/"), "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	server, err := h.Store.ServerByID(user.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if len(parts) == 2 {
		if parts[1] != "delete" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.Store.DeleteServer(user.ID, id); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		h.Agg.Trigger()
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	form := serverFormFromStored(*server, h.Store.CanStoreAPITokens())
	if r.Method == http.MethodGet {
		populateServerEditExtras(&form, h.Agg.Snapshot(), server.ID, h.Store, user.ID)
		h.render(w, r, "server_form.html", &pageData{Title: "Сервер", Section: "servers", Form: form})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	updated, replacementToken, err := parseServerForm(r)
	updated.ID = server.ID
	updated.HasAPIToken = server.HasAPIToken
	updated.CanStoreAPIToken = h.Store.CanStoreAPITokens()
	data := &pageData{Title: "Сервер", Section: "servers", Form: updated}
	if err != nil {
		h.renderServerFormError(w, r, data, http.StatusBadRequest, err.Error())
		return
	}

	candidate := *server
	candidate.Name = updated.Name
	candidate.APIURL = updated.APIURL
	candidate.Path = updated.Path
	candidate.HostOverride = updated.HostOverride
	candidate.InsecureSkipVerify = updated.InsecureSkipVerify
	connectionChanged := candidate.APIURL != server.APIURL ||
		candidate.Path != server.Path ||
		candidate.InsecureSkipVerify != server.InsecureSkipVerify

	checkToken := replacementToken
	if checkToken == "" && connectionChanged {
		if server.TokenError != nil || !server.HasAPIToken || server.APIToken == "" {
			h.renderServerFormError(w, r, data, http.StatusConflict, savedTokenConfigurationError().Message)
			return
		}
		checkToken = server.APIToken
	}
	if checkToken != "" {
		if _, err := h.checker.Check(r.Context(), candidate, checkToken); err != nil {
			h.renderServerFormError(w, r, data, http.StatusBadGateway, mapConnectionError(err).Message)
			return
		}
	}
	if replacementToken != "" {
		candidate.APIToken = replacementToken
	}
	if err := h.Store.UpdateServer(&candidate); err != nil {
		status, message := mapStorageSaveError(err)
		h.renderServerFormError(w, r, data, status, message)
		return
	}
	h.Agg.Trigger()
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func serverFormFromStored(server storage.Server, canStore bool) serverFormData {
	return serverFormData{
		ID: server.ID, Name: server.Name, APIURL: server.APIURL, Path: server.Path,
		HostOverride: server.HostOverride, InsecureSkipVerify: server.InsecureSkipVerify,
		HasAPIToken: server.HasAPIToken, CanStoreAPIToken: canStore,
		OnboardingCompleted: server.OnboardingCompleted,
		UsesHTTP:            strings.HasPrefix(strings.ToLower(server.APIURL), "http://"),
	}
}

func (h *Handler) renderServerFormError(w http.ResponseWriter, r *http.Request, data *pageData, status int, message string) {
	data.Error = message
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	h.render(w, r, "server_form.html", data)
}

func writeConnectionCheck(w http.ResponseWriter, status int, response connectionCheckResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func mapConnectionError(err error) connectionCheckResponse {
	switch {
	case xui.IsKind(err, xui.ErrorUnauthorized):
		return connectionCheckResponse{Code: "unauthorized", Message: "API-токен отклонён панелью"}
	case xui.IsKind(err, xui.ErrorUnsupportedVersion):
		return connectionCheckResponse{Code: "unsupported_version", Message: "Версия 3x-ui не поддерживается"}
	case xui.IsKind(err, xui.ErrorTransport):
		return connectionCheckResponse{Code: "transport", Message: "Не удалось подключиться к панели"}
	case xui.IsKind(err, xui.ErrorDecode):
		return connectionCheckResponse{Code: "decode", Message: "Панель вернула некорректный ответ"}
	case xui.IsKind(err, xui.ErrorAPI):
		return connectionCheckResponse{Code: "api", Message: "Панель отклонила запрос"}
	default:
		return connectionCheckResponse{Code: "connection_failed", Message: "Не удалось проверить подключение"}
	}
}

func savedTokenConfigurationError() connectionCheckResponse {
	return connectionCheckResponse{
		Code: "configuration", Message: "Сохранённый API-токен недоступен; замените его",
	}
}

func mapStorageSaveError(err error) (int, string) {
	if errors.Is(err, storage.ErrMasterKeyRequired) {
		return http.StatusConflict, "API-токен нельзя сохранить: настройте master_key"
	}
	return http.StatusInternalServerError, "Не удалось сохранить сервер"
}

// populateServerEditExtras наполняет форму списком inbound'ов сервера (из
// snapshot) и списком других серверов пользователя (для дропдауна копирования).
func populateServerEditExtras(form *serverFormData, snap *aggregator.Snapshot, serverID int64, store *storage.Store, userID int64) {
	for _, snapshotServer := range snap.Servers {
		if snapshotServer.ID != serverID {
			continue
		}
		if message := serverStateErrorLabel(snapshotServer.State); message != "" {
			form.InboundsErr = message
		}
		for _, inbound := range snapshotServer.Inbounds {
			if !strings.EqualFold(inbound.Protocol, "vless") {
				continue
			}
			form.Inbounds = append(form.Inbounds, serverEditInbound{
				ID: inbound.ID, Remark: inbound.Remark, Port: inbound.Port,
				Network: networkLabel(inbound.Network), Security: securityLabel(inbound.Security),
				SecurityCl: strings.ToLower(securityLabel(inbound.Security)), Enable: inbound.Enable,
			})
		}
		sort.Slice(form.Inbounds, func(i, j int) bool {
			if form.Inbounds[i].Port != form.Inbounds[j].Port {
				return form.Inbounds[i].Port < form.Inbounds[j].Port
			}
			return form.Inbounds[i].Remark < form.Inbounds[j].Remark
		})
		break
	}
	all, err := store.ListServersByUser(userID)
	if err != nil {
		return
	}
	for _, server := range all {
		if server.ID != serverID {
			form.OtherServers = append(form.OtherServers, serverOption{ID: server.ID, Name: server.Name})
		}
	}
	sort.Slice(form.OtherServers, func(i, j int) bool {
		return strings.ToLower(form.OtherServers[i].Name) < strings.ToLower(form.OtherServers[j].Name)
	})
}
