package webui

import (
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/ratelimit"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

//go:embed templates/*.html
var tmplFS embed.FS

type Handler struct {
	Cfg     *config.Config
	Store   *storage.Store
	Auth    *auth.Service
	Agg     *aggregator.Aggregator
	checker serverConnectionChecker

	// Лимиты против брутфорса /login и enumeration /sub/{prefix}/{key}.
	// Простой in-memory token-bucket по IP. Создаётся в New().
	loginLimiter *ratelimit.Limiter

	// Каждая страница — свой template set (base + только её content),
	// иначе `{{define "content"}}` перетирают друг друга.
	tmpls map[string]*template.Template
}

func New(cfg *config.Config, store *storage.Store, a *auth.Service, agg *aggregator.Aggregator) (*Handler, error) {
	funcs := template.FuncMap{
		"subSlug": subIDSlug,
	}
	pages := []string{"login.html", "register.html", "dashboard.html", "servers.html", "clients.html", "groups.html", "server_onboarding.html", "server_form.html", "admin.html"}
	tmpls := map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(tmplFS, "templates/base.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		tmpls[p] = t
	}
	return &Handler{
		Cfg: cfg, Store: store, Auth: a, Agg: agg, checker: xuiConnectionChecker{timeout: cfg.RequestTimeout}, tmpls: tmpls,
		// 10 попыток в минуту — пускает законных, режет брутфорс.
		loginLimiter: ratelimit.New(10, time.Minute, 10),
	}, nil
}

// subIDSlug делает безопасный для HTML id и URL-фрагмента строковый идентификатор
// из subId: лат. буквы (lowercase), цифры, '-', '_'; всё прочее → '-'.
func subIDSlug(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Mount навешивает UI-роуты на mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/login", h.loginLimiter.Wrap(h.login))
	mux.HandleFunc("/logout", h.Auth.RequireCSRFOnly(h.logout))
	mux.HandleFunc("/register", h.loginLimiter.Wrap(h.register))

	mux.HandleFunc("/dashboard", h.Auth.RequireUser(h.dashboard))
	mux.HandleFunc("/dashboard/servers", h.Auth.RequireUser(h.serversPage))
	mux.HandleFunc("/dashboard/clients", h.Auth.RequireUser(h.clientsPage))
	mux.HandleFunc("/dashboard/groups", h.Auth.RequireUser(h.groupsPage))
	mux.HandleFunc("/dashboard/groups/new", h.Auth.RequireUser(h.groupCreate))
	mux.HandleFunc("/dashboard/groups/", h.Auth.RequireUser(h.groupAction))
	mux.HandleFunc("/dashboard/onboarding/", h.Auth.RequireUser(h.serverOnboarding))
	mux.HandleFunc("/dashboard/servers/new", h.Auth.RequireUser(h.serverNew))
	mux.HandleFunc("/dashboard/servers/check", h.Auth.RequireUser(h.serverCheck))
	mux.HandleFunc("/dashboard/servers/", h.Auth.RequireUser(h.serverEdit))
	mux.HandleFunc("/dashboard/clients/inbound/add", h.Auth.RequireUser(h.clientInboundAdd))
	mux.HandleFunc("/dashboard/clients/inbound/remove", h.Auth.RequireUser(h.clientInboundRemove))
	mux.HandleFunc("/dashboard/inbounds/copy", h.Auth.RequireUser(h.inboundCopy))
	mux.HandleFunc("/dashboard/inbounds/edit", h.Auth.RequireUser(h.inboundEdit))
	mux.HandleFunc("/dashboard/inbounds/delete", h.Auth.RequireUser(h.inboundDelete))

	mux.HandleFunc("/admin", h.Auth.RequireAdmin(h.admin))
	mux.HandleFunc("/admin/invites/new", h.Auth.RequireAdmin(h.inviteCreate))
	mux.HandleFunc("/admin/invites/", h.Auth.RequireAdmin(h.inviteDelete))
	mux.HandleFunc("/admin/users/", h.Auth.RequireAdmin(h.userDelete))
}

type pageData struct {
	Title     string
	Section   string
	User      *storage.User
	Error     string
	Flash     string
	CSRFToken string
	// плюс произвольные поля:
	Form          any
	Dashboard     *dashboardData
	ClientsPage   *clientsPageData
	GroupsPage    *groupsPageData
	Servers       []serverRow
	Onboarding    *serverOnboardingData
	Invites       any
	Users         any
	InviteTTLDays int
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data *pageData) {
	if data == nil {
		data = &pageData{}
	}
	data.User = auth.FromContext(r.Context())
	// Гарантируем csrf-токен для всех POST-форм на странице.
	data.CSRFToken = h.Auth.EnsureCSRF(w, r)
	// Flash, выставленный предыдущим запросом, поднимаем в Error/Flash —
	// если хендлер сам не задал явное сообщение.
	if data.Error == "" && data.Flash == "" {
		if k, m, ok := h.consumeFlash(w, r); ok {
			switch k {
			case flashError:
				data.Error = m
			case flashSuccess:
				data.Flash = m
			}
		}
	}
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

// ---- flash (одноразовое сообщение через cookie) ----

type flashKind string

const (
	flashError   flashKind = "e"
	flashSuccess flashKind = "s"
	flashCookie            = "xuiagg_flash"
)

func (h *Handler) setFlash(w http.ResponseWriter, kind flashKind, msg string) {
	val := string(kind) + "|" + msg
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.URLEncoding.EncodeToString([]byte(val)),
		Path:     "/",
		MaxAge:   60,
		HttpOnly: true,
		Secure:   h.Cfg.CookiesSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// consumeFlash читает cookie и сразу истекает её, чтобы flash показалось ровно один раз.
func (h *Handler) consumeFlash(w http.ResponseWriter, r *http.Request) (flashKind, string, bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return "", "", false
	}
	raw, err := base64.URLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.Cfg.CookiesSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return flashKind(parts[0]), parts[1], true
}

// flashErrAndRedirect — короткий путь для ошибок мутаций: ставит flash и
// редиректит на указанный URL (303 See Other, чтобы браузер сделал GET).
func (h *Handler) flashErrAndRedirect(w http.ResponseWriter, r *http.Request, msg, target string) {
	h.setFlash(w, flashError, msg)
	http.Redirect(w, r, target, http.StatusSeeOther)
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
	ID           int64
	Name         string
	APIURL       string
	PublicHost   string
	ClientCount  int
	FetchedAt    string
	PanelVersion string
	StateLabel   string
	StateClass   string
}

type inboundView struct {
	Network    string // TCP / WS / GRPC / XHTTP
	Security   string // Reality / TLS / none
	SecurityCl string // reality / tls / none — для CSS-класса
	Port       int
	Remark     string
	Enabled    bool

	ServerID  int64
	InboundID int
}

type serverInbounds struct {
	Name     string
	Inbounds []inboundView
}

type candidateInbound struct {
	ServerID   int64
	ServerName string
	InboundID  int
	Network    string
	Security   string
	SecurityCl string
	Port       int
	Remark     string
}

type clientCard struct {
	Sub        string
	Emails     string // email'ы inbound'ов под этим subId, через запятую
	SubURL     string // полный URL подписки
	Servers    []serverInbounds
	Candidates []candidateInbound // inbound'ы пользователя, в которых ещё нет subId
}

type dashboardData struct {
	Servers         []serverRow
	Cards           []clientCard
	SubBase         string
	RefreshInterval string
	TotalClients    int
	TotalGroups     int
	ProblemServers  int
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
	for _, s := range servers {
		row := serverRow{
			ID: s.ID, Name: s.Name, APIURL: s.APIURL, PublicHost: s.HostOverride,
			StateLabel: "ожидание",
		}
		if st, ok := statByID[s.ID]; ok {
			row.PublicHost = st.PublicHost
			for _, group := range st.Groups {
				row.ClientCount += len(group.Records)
			}
			if !st.FetchedAt.IsZero() {
				row.FetchedAt = st.FetchedAt.Format("15:04:05")
			}
			row.PanelVersion = panelVersionLabel(st.PanelVersion)
			row.StateLabel, row.StateClass = serverStatePresentation(st.State)
		}
		rows = append(rows, row)
	}

	subBase := h.subscriptionBase(r, u)
	cards := buildClientCards(snap, u.ID, subBase)
	groups, err := h.Store.ListClientGroups(u.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	problemServers := 0
	for _, row := range rows {
		if row.StateClass == "err" {
			problemServers++
		}
	}

	data := &pageData{
		Title:   "Обзор",
		Section: "dashboard",
		Dashboard: &dashboardData{
			Servers:         rows,
			Cards:           cards,
			SubBase:         subBase,
			RefreshInterval: h.Cfg.RefreshInterval.String(),
			TotalClients:    len(cards),
			TotalGroups:     len(groups),
			ProblemServers:  problemServers,
		},
	}
	h.render(w, r, "dashboard.html", data)
}

func (h *Handler) serversPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())
	servers, err := h.Store.ListServersByUser(u.ID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	snap := h.Agg.Snapshot()
	statByID := make(map[int64]aggregator.ServerSnapshot, len(snap.Servers))
	for _, state := range snap.Servers {
		statByID[state.ID] = state
	}
	rows := make([]serverRow, 0, len(servers))
	for _, server := range servers {
		row := serverRow{ID: server.ID, Name: server.Name, APIURL: server.APIURL, PublicHost: server.HostOverride, StateLabel: "ожидание"}
		if state, ok := statByID[server.ID]; ok {
			row.PublicHost = state.PublicHost
			for _, group := range state.Groups {
				row.ClientCount += len(group.Records)
			}
			if !state.FetchedAt.IsZero() {
				row.FetchedAt = state.FetchedAt.Format("15:04:05")
			}
			row.PanelVersion = panelVersionLabel(state.PanelVersion)
			row.StateLabel, row.StateClass = serverStatePresentation(state.State)
		}
		rows = append(rows, row)
	}
	h.render(w, r, "servers.html", &pageData{Title: "Серверы", Section: "servers", Servers: rows})
}

// buildClientCards joins native client groups to native inbound summaries. All
// exact memberships participate in candidate suppression, including duplicate,
// disabled and orphan records. Candidates remain VLESS-only for Task 8.
func buildClientCards(snap *aggregator.Snapshot, userID int64, subBase string) []clientCard {
	type serverData struct {
		name        string
		inbounds    []aggregator.InboundInfo
		inboundByID map[int]aggregator.InboundInfo
	}
	userServers := map[int64]*serverData{}
	for _, srv := range snap.Servers {
		if srv.UserID != userID {
			continue
		}
		data := &serverData{
			name: srv.Name, inbounds: srv.Inbounds,
			inboundByID: make(map[int]aggregator.InboundInfo, len(srv.Inbounds)),
		}
		for _, inbound := range srv.Inbounds {
			data.inboundByID[inbound.ID] = inbound
		}
		userServers[srv.ID] = data
	}

	type serverAcc struct {
		name        string
		inbounds    map[int]inboundView
		memberships map[int]struct{}
	}
	type subAcc struct {
		emails  map[string]struct{}
		servers map[int64]*serverAcc
		sortKey string
	}
	bySub := map[string]*subAcc{}

	for _, srv := range snap.Servers {
		if srv.UserID != userID {
			continue
		}
		server := userServers[srv.ID]
		for subID, group := range srv.Groups {
			if subID == "" {
				continue
			}
			a, ok := bySub[subID]
			if !ok {
				a = &subAcc{
					emails:  map[string]struct{}{},
					servers: map[int64]*serverAcc{},
				}
				bySub[subID] = a
			}
			sa, ok := a.servers[srv.ID]
			if !ok {
				sa = &serverAcc{
					name: srv.Name, inbounds: map[int]inboundView{}, memberships: map[int]struct{}{},
				}
				a.servers[srv.ID] = sa
			}
			for _, record := range group.Records {
				if record.Email != "" {
					a.emails[record.Email] = struct{}{}
				}
				for _, inboundID := range record.InboundIDs {
					sa.memberships[inboundID] = struct{}{}
					inbound, found := server.inboundByID[inboundID]
					if !found {
						continue
					}
					view, exists := sa.inbounds[inboundID]
					if !exists {
						security := securityLabel(inbound.Security)
						view = inboundView{
							Network: networkLabel(inbound.Network), Security: security,
							SecurityCl: strings.ToLower(security), Port: inbound.Port, Remark: inbound.Remark,
							ServerID: srv.ID, InboundID: inbound.ID,
						}
					}
					view.Enabled = view.Enabled || (record.Enabled && inbound.Enable)
					sa.inbounds[inboundID] = view
				}
			}
		}
	}

	cards := make([]clientCard, 0, len(bySub))
	for sub, a := range bySub {
		emails := make([]string, 0, len(a.emails))
		for em := range a.emails {
			emails = append(emails, em)
		}
		sort.Strings(emails)

		serverList := make([]serverInbounds, 0, len(a.servers))
		for _, sa := range a.servers {
			inbounds := make([]inboundView, 0, len(sa.inbounds))
			for _, inbound := range sa.inbounds {
				inbounds = append(inbounds, inbound)
			}
			if len(inbounds) == 0 {
				continue
			}
			sort.Slice(inbounds, func(i, j int) bool {
				if inbounds[i].Port != inbounds[j].Port {
					return inbounds[i].Port < inbounds[j].Port
				}
				return inbounds[i].Remark < inbounds[j].Remark
			})
			serverList = append(serverList, serverInbounds{Name: sa.name, Inbounds: inbounds})
		}
		sort.Slice(serverList, func(i, j int) bool {
			return strings.ToLower(serverList[i].Name) < strings.ToLower(serverList[j].Name)
		})

		var candidates []candidateInbound
		for sid, sd := range userServers {
			sa := a.servers[sid]
			for _, ib := range sd.inbounds {
				if !ib.Enable || !strings.EqualFold(ib.Protocol, "vless") {
					continue
				}
				if sa != nil {
					if _, present := sa.memberships[ib.ID]; present {
						continue
					}
				}
				candidates = append(candidates, candidateInbound{
					ServerID:   sid,
					ServerName: sd.name,
					InboundID:  ib.ID,
					Network:    networkLabel(ib.Network),
					Security:   securityLabel(ib.Security),
					SecurityCl: strings.ToLower(securityLabel(ib.Security)),
					Port:       ib.Port,
					Remark:     ib.Remark,
				})
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			ai := strings.ToLower(candidates[i].ServerName)
			aj := strings.ToLower(candidates[j].ServerName)
			if ai != aj {
				return ai < aj
			}
			return candidates[i].Port < candidates[j].Port
		})

		card := clientCard{
			Sub:        sub,
			SubURL:     subBase + "/" + sub,
			Servers:    serverList,
			Candidates: candidates,
		}
		if !(len(emails) == 1 && emails[0] == sub) {
			card.Emails = strings.Join(emails, ", ")
		}
		if len(emails) > 0 {
			a.sortKey = strings.ToLower(emails[0])
		} else {
			a.sortKey = strings.ToLower(sub)
		}
		cards = append(cards, card)
	}

	sort.SliceStable(cards, func(i, j int) bool {
		ki := bySub[cards[i].Sub].sortKey
		kj := bySub[cards[j].Sub].sortKey
		if ki != kj {
			return ki < kj
		}
		return cards[i].Sub < cards[j].Sub
	})
	return cards
}

func serverStatePresentation(state aggregator.ServerState) (label, class string) {
	switch state {
	case aggregator.ServerOK:
		return "ok", "ok"
	case aggregator.ServerDegraded:
		return "частично доступна", "err"
	case aggregator.ServerTokenRequired:
		return "требуется API-токен", "err"
	case aggregator.ServerTokenRejected:
		return "API-токен отклонён", "err"
	case aggregator.ServerUnsupportedVersion:
		return "версия 3x-ui не поддерживается", "err"
	case aggregator.ServerUnavailable:
		return "Панель временно недоступна", "err"
	case aggregator.ServerConfigurationError:
		return "ошибка настройки API-токена", "err"
	default:
		return "ожидание", ""
	}
}

func serverStateErrorLabel(state aggregator.ServerState) string {
	switch state {
	case aggregator.ServerTokenRequired, aggregator.ServerTokenRejected,
		aggregator.ServerUnsupportedVersion, aggregator.ServerUnavailable,
		aggregator.ServerConfigurationError:
		label, _ := serverStatePresentation(state)
		return label
	default:
		return ""
	}
}

func panelVersionLabel(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return ""
	}
	for _, part := range parts {
		if part == "" || len(part) > 4 {
			return ""
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return ""
			}
		}
	}
	return "3x-ui " + version
}

func networkLabel(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	if n == "" {
		n = "tcp"
	}
	return strings.ToUpper(n)
}

func securityLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "reality":
		return "Reality"
	case "tls":
		return "TLS"
	case "", "none":
		return "none"
	default:
		return s
	}
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
