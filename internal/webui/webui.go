package webui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

//go:embed templates/*.html
var tmplFS embed.FS

type Handler struct {
	Cfg   *config.Config
	Store *storage.Store
	Auth  *auth.Service
	Agg   *aggregator.Aggregator

	// Каждая страница — свой template set (base + только её content),
	// иначе `{{define "content"}}` перетирают друг друга.
	tmpls map[string]*template.Template
}

func New(cfg *config.Config, store *storage.Store, a *auth.Service, agg *aggregator.Aggregator) (*Handler, error) {
	pages := []string{"login.html", "register.html", "dashboard.html", "server_form.html", "admin.html"}
	tmpls := map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New("").ParseFS(tmplFS, "templates/base.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		tmpls[p] = t
	}
	return &Handler{Cfg: cfg, Store: store, Auth: a, Agg: agg, tmpls: tmpls}, nil
}

// Mount навешивает UI-роуты на mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/login", h.login)
	mux.HandleFunc("/logout", h.logout)
	mux.HandleFunc("/register", h.register)

	mux.HandleFunc("/dashboard", h.Auth.RequireUser(h.dashboard))
	mux.HandleFunc("/dashboard/servers/new", h.Auth.RequireUser(h.serverNew))
	mux.HandleFunc("/dashboard/servers/", h.Auth.RequireUser(h.serverEdit))

	mux.HandleFunc("/admin", h.Auth.RequireAdmin(h.admin))
	mux.HandleFunc("/admin/invites/new", h.Auth.RequireAdmin(h.inviteCreate))
	mux.HandleFunc("/admin/invites/", h.Auth.RequireAdmin(h.inviteDelete))
	mux.HandleFunc("/admin/users/", h.Auth.RequireAdmin(h.userDelete))
}

type pageData struct {
	Title   string
	Section string
	User    *storage.User
	Error   string
	Flash   string
	// плюс произвольные поля:
	Form          any
	Dashboard     *dashboardData
	Invites       any
	Users         any
	InviteTTLDays int
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data *pageData) {
	if data == nil {
		data = &pageData{}
	}
	data.User = auth.FromContext(r.Context())
	t, ok := h.tmpls[name]
	if !ok {
		log.Printf("webui: unknown template %q", name)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("webui: render %s: %v", name, err)
	}
}

func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	u := auth.FromContext(r.Context())
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// ---- login / logout / register ----

type loginForm struct{ Login string }

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if u := auth.FromContext(r.Context()); u != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	data := &pageData{Title: "Вход", Form: loginForm{}}
	if r.Method == http.MethodPost {
		login := strings.TrimSpace(r.FormValue("login"))
		pw := r.FormValue("password")
		data.Form = loginForm{Login: login}
		u, err := h.Auth.Authenticate(login, pw)
		if err != nil {
			data.Error = "Неверный логин или пароль"
			w.WriteHeader(http.StatusUnauthorized)
			h.render(w, r, "login.html", data)
			return
		}
		if err := h.Auth.StartSession(w, u.ID); err != nil {
			data.Error = "Ошибка: " + err.Error()
			h.render(w, r, "login.html", data)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	h.render(w, r, "login.html", data)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.Auth.EndSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type registerForm struct {
	Token         string
	Login         string
	InviteExpires string
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if u := auth.FromContext(r.Context()); u != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		http.Error(w, "invite token required", http.StatusBadRequest)
		return
	}
	inv, err := h.Auth.ValidateInvite(token)
	if err != nil {
		http.Error(w, "Приглашение недействительно или истекло.", http.StatusForbidden)
		return
	}

	form := registerForm{Token: token, InviteExpires: inv.ExpiresAt.Format("2006-01-02 15:04")}
	data := &pageData{Title: "Регистрация", Form: form}

	if r.Method == http.MethodPost {
		login := strings.TrimSpace(r.FormValue("login"))
		pw := r.FormValue("password")
		pw2 := r.FormValue("password2")
		form.Login = login
		data.Form = form

		if len(login) < 3 || len(login) > 32 {
			data.Error = "Логин должен быть 3–32 символа"
			h.render(w, r, "register.html", data)
			return
		}
		if !validLogin(login) {
			data.Error = "Логин может содержать только латиницу, цифры, _ . -"
			h.render(w, r, "register.html", data)
			return
		}
		if len(pw) < 8 {
			data.Error = "Пароль должен быть не короче 8 символов"
			h.render(w, r, "register.html", data)
			return
		}
		if pw != pw2 {
			data.Error = "Пароли не совпадают"
			h.render(w, r, "register.html", data)
			return
		}
		if existing, _ := h.Store.UserByLogin(login); existing != nil {
			data.Error = "Пользователь с таким логином уже существует"
			h.render(w, r, "register.html", data)
			return
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			data.Error = "Не удалось захешировать пароль"
			h.render(w, r, "register.html", data)
			return
		}
		user, err := h.Store.CreateUser(login, hash, false)
		if err != nil {
			data.Error = "Не удалось создать пользователя: " + err.Error()
			h.render(w, r, "register.html", data)
			return
		}
		_ = h.Store.MarkInviteUsed(token, user.ID)
		if err := h.Auth.StartSession(w, user.ID); err != nil {
			data.Error = err.Error()
			h.render(w, r, "register.html", data)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	h.render(w, r, "register.html", data)
}

func validLogin(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// ---- dashboard / servers ----

type serverRow struct {
	ID         int64
	Name       string
	APIURL     string
	PublicHost string
	EntryCount int
	FetchedAt  string
	Err        string
}

type clientRow struct {
	Email   string
	Present []bool // параллельно с dashboardData.ServerNames
}

type dashboardData struct {
	Servers         []serverRow
	ServerNames     []string
	Clients         []clientRow
	SubBase         string
	RefreshInterval string
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	servers, err := h.Store.ListServersByUser(u.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	snap := h.Agg.Snapshot()
	statByID := map[int64]aggregator.ServerSnapshot{}
	for _, s := range snap.Servers {
		statByID[s.ID] = s
	}

	var rows []serverRow
	serverNames := make([]string, 0, len(servers))
	serverIDs := make([]int64, 0, len(servers))
	for _, s := range servers {
		row := serverRow{ID: s.ID, Name: s.Name, APIURL: s.APIURL, PublicHost: s.HostOverride}
		if st, ok := statByID[s.ID]; ok {
			row.PublicHost = st.PublicHost
			row.EntryCount = len(st.Entries)
			if !st.FetchedAt.IsZero() {
				row.FetchedAt = st.FetchedAt.Format("15:04:05")
			}
			if st.Err != nil {
				row.Err = st.Err.Error()
			}
		}
		rows = append(rows, row)
		serverNames = append(serverNames, s.Name)
		serverIDs = append(serverIDs, s.ID)
	}

	// Матрица email × server. Учитываем только включённые записи.
	presence := map[string]map[int64]bool{}
	for _, srv := range snap.Servers {
		if srv.UserID != u.ID {
			continue
		}
		for _, e := range srv.Entries {
			if !e.Enabled {
				continue
			}
			if presence[e.Email] == nil {
				presence[e.Email] = map[int64]bool{}
			}
			presence[e.Email][srv.ID] = true
		}
	}
	emails := make([]string, 0, len(presence))
	for em := range presence {
		emails = append(emails, em)
	}
	sort.Strings(emails)

	clients := make([]clientRow, 0, len(emails))
	for _, em := range emails {
		row := clientRow{Email: em, Present: make([]bool, len(serverIDs))}
		for i, sid := range serverIDs {
			row.Present[i] = presence[em][sid]
		}
		clients = append(clients, row)
	}

	data := &pageData{
		Title:   "Мои серверы",
		Section: "dashboard",
		Dashboard: &dashboardData{
			Servers:         rows,
			ServerNames:     serverNames,
			Clients:         clients,
			SubBase:         h.subscriptionBase(r, u),
			RefreshInterval: h.Cfg.RefreshInterval.String(),
		},
	}
	h.render(w, r, "dashboard.html", data)
}

type serverFormData struct {
	ID                 int64
	Name               string
	APIURL             string
	Path               string
	Username           string
	HostOverride       string
	InsecureSkipVerify bool
}

func (h *Handler) serverNew(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	data := &pageData{Title: "Новый сервер", Section: "dashboard", Form: serverFormData{Path: "/"}}
	if r.Method == http.MethodPost {
		form, err := parseServerForm(r)
		if err != nil {
			data.Form = form
			data.Error = err.Error()
			h.render(w, r, "server_form.html", data)
			return
		}
		pw := r.FormValue("password")
		if pw == "" {
			data.Form = form
			data.Error = "Пароль обязателен"
			h.render(w, r, "server_form.html", data)
			return
		}
		srv := &storage.Server{
			UserID: u.ID, Name: form.Name, APIURL: form.APIURL, Path: form.Path,
			Username: form.Username, Password: pw,
			InsecureSkipVerify: form.InsecureSkipVerify, HostOverride: form.HostOverride,
		}
		if _, err := h.Store.CreateServer(srv); err != nil {
			data.Form = form
			data.Error = "Не удалось создать сервер: " + err.Error()
			h.render(w, r, "server_form.html", data)
			return
		}
		h.Agg.Trigger()
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	h.render(w, r, "server_form.html", data)
}

func (h *Handler) serverEdit(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())

	// /dashboard/servers/{id}            (GET, POST)
	// /dashboard/servers/{id}/delete     (POST)
	rest := strings.TrimPrefix(r.URL.Path, "/dashboard/servers/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srv, err := h.Store.ServerByID(u.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if len(parts) == 2 && parts[1] == "delete" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.Store.DeleteServer(u.ID, id); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		h.Agg.Trigger()
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	form := serverFormData{
		ID: srv.ID, Name: srv.Name, APIURL: srv.APIURL, Path: srv.Path,
		Username: srv.Username, HostOverride: srv.HostOverride, InsecureSkipVerify: srv.InsecureSkipVerify,
	}
	data := &pageData{Title: "Сервер", Section: "dashboard", Form: form}

	if r.Method == http.MethodPost {
		updated, err := parseServerForm(r)
		if err != nil {
			updated.ID = srv.ID
			data.Form = updated
			data.Error = err.Error()
			h.render(w, r, "server_form.html", data)
			return
		}
		srv.Name = updated.Name
		srv.APIURL = updated.APIURL
		srv.Path = updated.Path
		srv.Username = updated.Username
		srv.HostOverride = updated.HostOverride
		srv.InsecureSkipVerify = updated.InsecureSkipVerify
		if pw := r.FormValue("password"); pw != "" {
			srv.Password = pw
		}
		if err := h.Store.UpdateServer(srv); err != nil {
			updated.ID = srv.ID
			data.Form = updated
			data.Error = "Не удалось сохранить: " + err.Error()
			h.render(w, r, "server_form.html", data)
			return
		}
		h.Agg.Trigger()
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	h.render(w, r, "server_form.html", data)
}

func parseServerForm(r *http.Request) (serverFormData, error) {
	f := serverFormData{
		Name:               strings.TrimSpace(r.FormValue("name")),
		APIURL:             strings.TrimSpace(r.FormValue("api_url")),
		Path:               strings.TrimSpace(r.FormValue("path")),
		Username:           strings.TrimSpace(r.FormValue("username")),
		HostOverride:       strings.TrimSpace(r.FormValue("host_override")),
		InsecureSkipVerify: r.FormValue("insecure_skip_verify") == "1",
	}
	if f.Name == "" || f.APIURL == "" || f.Username == "" {
		return f, errors.New("name, api_url и username обязательны")
	}
	if _, err := url.Parse(f.APIURL); err != nil {
		return f, errors.New("некорректный api_url")
	}
	if f.Path == "" {
		f.Path = "/"
	}
	if !strings.HasPrefix(f.Path, "/") {
		f.Path = "/" + f.Path
	}
	if !strings.HasSuffix(f.Path, "/") {
		f.Path += "/"
	}
	return f, nil
}

// ---- admin ----

type inviteRow struct {
	Token   string
	URL     string
	Expires string
	Expired bool
	Used    bool
	UsedBy  string
}

type userRow struct {
	ID          int64
	Login       string
	IsAdmin     bool
	ServerCount int
	Created     string
}

func (h *Handler) admin(w http.ResponseWriter, r *http.Request) {
	users, err := h.Store.ListUsers()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	invites, err := h.Store.ListInvites()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	allServers, err := h.Store.ListAllServers()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	serversByUser := map[int64]int{}
	for _, s := range allServers {
		serversByUser[s.UserID]++
	}
	userByID := map[int64]string{}
	var urows []userRow
	for _, u := range users {
		urows = append(urows, userRow{
			ID: u.ID, Login: u.Login, IsAdmin: u.IsAdmin,
			ServerCount: serversByUser[u.ID],
			Created:     u.CreatedAt.Format("2006-01-02"),
		})
		userByID[u.ID] = u.Login
	}

	base := h.externalBase(r)
	now := time.Now()
	var irows []inviteRow
	for _, inv := range invites {
		row := inviteRow{
			Token:   inv.Token,
			URL:     base + "/register?token=" + inv.Token,
			Expires: inv.ExpiresAt.Format("2006-01-02 15:04"),
		}
		if inv.UsedAt != nil {
			row.Used = true
			if inv.UsedBy != nil {
				row.UsedBy = userByID[*inv.UsedBy]
			}
		} else if now.After(inv.ExpiresAt) {
			row.Expired = true
		}
		irows = append(irows, row)
	}

	data := &pageData{
		Title: "Админ", Section: "admin",
		Users: urows, Invites: irows,
		InviteTTLDays: int(auth.InviteTTL / (24 * time.Hour)),
	}
	h.render(w, r, "admin.html", data)
}

func (h *Handler) inviteCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())
	if _, err := h.Store.CreateInvite(u.ID, auth.InviteTTL); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) inviteDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/invites/")
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "delete" {
		http.NotFound(w, r)
		return
	}
	_ = h.Store.DeleteInvite(parts[0])
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) userDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	me := auth.FromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "delete" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if id == me.ID {
		http.Error(w, "нельзя удалить себя", http.StatusBadRequest)
		return
	}
	target, err := h.Store.UserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if target.IsAdmin {
		http.Error(w, "нельзя удалить администратора", http.StatusBadRequest)
		return
	}
	_ = h.Store.DeleteUser(id)
	h.Agg.Trigger()
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---- helpers ----

// externalBase возвращает базовый URL для ссылок наружу. Предпочитает public_url из конфига,
// иначе пытается собрать из заголовков запроса (с учётом proxy).
func (h *Handler) externalBase(r *http.Request) string {
	if h.Cfg.PublicURL != "" {
		return strings.TrimRight(h.Cfg.PublicURL, "/")
	}
	scheme := "http"
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

func (h *Handler) subscriptionBase(r *http.Request, u *storage.User) string {
	base := h.externalBase(r) + "/sub"
	if u.SubPrefix != "" {
		base += "/" + u.SubPrefix
	}
	return base
}
