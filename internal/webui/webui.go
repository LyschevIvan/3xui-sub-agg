package webui

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
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
	"github.com/LyschevIvan/3xui-sub-agg/internal/ratelimit"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

//go:embed templates/*.html
var tmplFS embed.FS

type Handler struct {
	Cfg   *config.Config
	Store *storage.Store
	Auth  *auth.Service
	Agg   *aggregator.Aggregator

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
	pages := []string{"login.html", "register.html", "dashboard.html", "server_form.html", "admin.html"}
	tmpls := map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(tmplFS, "templates/base.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		tmpls[p] = t
	}
	return &Handler{
		Cfg: cfg, Store: store, Auth: a, Agg: agg, tmpls: tmpls,
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
	mux.HandleFunc("/dashboard/servers/new", h.Auth.RequireUser(h.serverNew))
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
	ID         int64
	Name       string
	APIURL     string
	PublicHost string
	EntryCount int
	FetchedAt  string
	Err        string
}

type inboundView struct {
	Network    string // TCP / WS / GRPC / XHTTP
	Security   string // Reality / TLS / none
	SecurityCl string // reality / tls / none — для CSS-класса
	Port       int
	Remark     string

	// Для кнопки "удалить из inbound'а"
	ServerID   int64
	InboundID  int
	ClientUUID string
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
	}

	subBase := h.subscriptionBase(r, u)
	cards := buildClientCards(snap, u.ID, subBase)

	data := &pageData{
		Title:   "Мои серверы",
		Section: "dashboard",
		Dashboard: &dashboardData{
			Servers:         rows,
			Cards:           cards,
			SubBase:         subBase,
			RefreshInterval: h.Cfg.RefreshInterval.String(),
		},
	}
	h.render(w, r, "dashboard.html", data)
}

// buildClientCards собирает по одной карточке на каждый subId пользователя.
// Внутри карточки — сервера, на которых клиент присутствует, и список inbound'ов
// каждого сервера (с дедупом по InboundID). В Candidates кладём enabled
// vless-inbound'ы серверов пользователя, в которых subId ещё нет — для кнопки
// «+ Добавить».
func buildClientCards(snap *aggregator.Snapshot, userID int64, subBase string) []clientCard {
	// Серверы пользователя с полными списками inbound'ов — нужны для кандидатов
	// (включая inbound'ы, в которых ни одного клиента ещё нет).
	type serverData struct {
		name     string
		inbounds []aggregator.InboundInfo
	}
	userServers := map[int64]*serverData{}
	for _, srv := range snap.Servers {
		if srv.UserID != userID {
			continue
		}
		userServers[srv.ID] = &serverData{name: srv.Name, inbounds: srv.Inbounds}
	}

	type serverAcc struct {
		name     string
		inbounds []inboundView
		seen     map[int]bool
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
		for _, e := range srv.Entries {
			if !e.Enabled || e.SubID == "" {
				continue
			}
			a, ok := bySub[e.SubID]
			if !ok {
				a = &subAcc{
					emails:  map[string]struct{}{},
					servers: map[int64]*serverAcc{},
				}
				bySub[e.SubID] = a
			}
			a.emails[e.Email] = struct{}{}

			sa, ok := a.servers[e.ServerID]
			if !ok {
				sa = &serverAcc{name: e.ServerName, seen: map[int]bool{}}
				a.servers[e.ServerID] = sa
			}
			if sa.seen[e.InboundID] {
				continue
			}
			sa.seen[e.InboundID] = true
			sa.inbounds = append(sa.inbounds, inboundView{
				Network:    networkLabel(e.Network),
				Security:   securityLabel(e.Security),
				SecurityCl: strings.ToLower(securityLabel(e.Security)),
				Port:       e.Port,
				Remark:     e.InboundRemark,
				ServerID:   e.ServerID,
				InboundID:  e.InboundID,
				ClientUUID: e.ClientUUID,
			})
		}
	}

	cards := make([]clientCard, 0, len(bySub))
	for sub, a := range bySub {
		emails := make([]string, 0, len(a.emails))
		for em := range a.emails {
			emails = append(emails, em)
		}
		sort.Strings(emails)

		// Сортируем сервера внутри карточки по имени; inbound'ы — по порту.
		serverList := make([]serverInbounds, 0, len(a.servers))
		for _, sa := range a.servers {
			sort.Slice(sa.inbounds, func(i, j int) bool {
				if sa.inbounds[i].Port != sa.inbounds[j].Port {
					return sa.inbounds[i].Port < sa.inbounds[j].Port
				}
				return sa.inbounds[i].Remark < sa.inbounds[j].Remark
			})
			serverList = append(serverList, serverInbounds{Name: sa.name, Inbounds: sa.inbounds})
		}
		sort.Slice(serverList, func(i, j int) bool {
			return strings.ToLower(serverList[i].Name) < strings.ToLower(serverList[j].Name)
		})

		// Кандидаты: enabled inbound'ы серверов пользователя, в которых нет subId.
		var candidates []candidateInbound
		for sid, sd := range userServers {
			sa := a.servers[sid]
			for _, ib := range sd.inbounds {
				if !ib.Enable {
					continue
				}
				if sa != nil && sa.seen[ib.ID] {
					continue
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
		// Email-подсказку прячем, если subId совпадает с единственным email.
		if !(len(emails) == 1 && emails[0] == sub) {
			card.Emails = strings.Join(emails, ", ")
		}
		// Сортировка карточек: по первому email (человекочитаемому), затем по subId.
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

// ---- client / inbound mutations ----

// POST /dashboard/clients/inbound/add
// form: sub_id, server_id, inbound_id, email (optional)
func (h *Handler) clientInboundAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())

	subID := strings.TrimSpace(r.FormValue("sub_id"))
	serverID, _ := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, _ := strconv.Atoi(r.FormValue("inbound_id"))
	email := strings.TrimSpace(r.FormValue("email"))
	if subID == "" || serverID == 0 || inboundID == 0 {
		http.Error(w, "bad request: sub_id, server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}

	back := "/dashboard#add-" + subIDSlug(subID)

	srv, err := h.Store.ServerByID(u.ID, serverID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}

	// Находим inbound в snapshot — нужно для определения flow по network/security
	// и для дефолтного email.
	snap := h.Agg.Snapshot()
	var info *aggregator.InboundInfo
	for _, ssrv := range snap.Servers {
		if ssrv.ID != serverID {
			continue
		}
		for i := range ssrv.Inbounds {
			if ssrv.Inbounds[i].ID == inboundID {
				info = &ssrv.Inbounds[i]
				break
			}
		}
	}
	if info == nil {
		h.flashErrAndRedirect(w, r, "Inbound не найден в snapshot — попробуйте через минуту", back)
		return
	}

	if email == "" {
		// subId + имя inbound'а — чтобы при добавлении одного subId в несколько
		// inbound'ов (в т.ч. на одном сервере) email'ы оставались уникальны и
		// читались в панели 3x-ui.
		email = defaultClientEmail(subID, info.Remark, info.ID)
	}

	uuid, err := newClientUUID()
	if err != nil {
		h.flashErrAndRedirect(w, r, "uuid: "+err.Error(), back)
		return
	}

	xc, err := h.Agg.XuiClient(*srv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui: "+err.Error(), back)
		return
	}
	err = xc.AddClient(r.Context(), inboundID, xui.InboundClient{
		ID:     uuid,
		Email:  email,
		Flow:   xui.VisionFlow(info.Network, info.Security),
		Enable: true,
		SubID:  subID,
	})
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось добавить клиента: "+err.Error(), back)
		return
	}
	h.Agg.RefreshNow(r.Context())
	h.setFlash(w, flashSuccess, fmt.Sprintf("Клиент добавлен в inbound %q", info.Remark))
	// Возвращаем пользователя к развёрнутому списку «+ Добавить» этой карточки —
	// удобно, когда добавляют сразу несколько inbound'ов подряд.
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// POST /dashboard/clients/inbound/remove
// form: server_id, inbound_id, client_uuid, sub_id (для anchor-редиректа)
func (h *Handler) clientInboundRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())

	serverID, _ := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, _ := strconv.Atoi(r.FormValue("inbound_id"))
	clientUUID := strings.TrimSpace(r.FormValue("client_uuid"))
	subID := strings.TrimSpace(r.FormValue("sub_id"))
	if serverID == 0 || inboundID == 0 || clientUUID == "" {
		http.Error(w, "bad request: server_id, inbound_id, client_uuid обязательны", http.StatusBadRequest)
		return
	}

	back := "/dashboard"
	if subID != "" {
		back += "#card-" + subIDSlug(subID)
	}

	srv, err := h.Store.ServerByID(u.ID, serverID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}

	xc, err := h.Agg.XuiClient(*srv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui: "+err.Error(), back)
		return
	}
	if err := xc.DeleteClient(r.Context(), inboundID, clientUUID); err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось убрать клиента: "+err.Error(), back)
		return
	}
	h.Agg.RefreshNow(r.Context())
	h.setFlash(w, flashSuccess, "Клиент убран из inbound'а")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// defaultClientEmail формирует email для нового клиента: subId + имя inbound'а.
// Email — лейбл клиента в панели 3x-ui, должен быть уникален в пределах одного
// inbound'а. Подмешивая имя inbound'а, мы получаем уникальность даже когда
// один subId назначен на несколько inbound'ов одного сервера.
func defaultClientEmail(subID, inboundRemark string, inboundID int) string {
	name := strings.TrimSpace(inboundRemark)
	if name == "" {
		name = "inbound" + strconv.Itoa(inboundID)
	}
	return subID + "-" + name
}

// newClientUUID — UUID v4 без внешних зависимостей.
func newClientUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// POST /dashboard/inbounds/copy
// form: source_server_id, source_inbound_id, target_server_id, new_remark, new_port
//
// Тянет full inbound с источника, перегенерирует UUID у клиентов и
// переименовывает их email в "subId-{remark}" (используя итоговое имя
// inbound'а), вызывает AddInbound на целевом сервере. target_server_id может
// совпадать с source_server_id (копия на тот же сервер) — но тогда port обязан
// отличаться, иначе 3x-ui откажет.
func (h *Handler) inboundCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())

	sourceServerID, _ := strconv.ParseInt(r.FormValue("source_server_id"), 10, 64)
	sourceInboundID, _ := strconv.Atoi(r.FormValue("source_inbound_id"))
	targetServerID, _ := strconv.ParseInt(r.FormValue("target_server_id"), 10, 64)
	newRemark := strings.TrimSpace(r.FormValue("new_remark"))
	newPort, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("new_port")))
	if sourceServerID == 0 || sourceInboundID == 0 || targetServerID == 0 {
		http.Error(w, "bad request: source_server_id, source_inbound_id, target_server_id обязательны", http.StatusBadRequest)
		return
	}
	back := "/dashboard/servers/" + strconv.FormatInt(sourceServerID, 10) + "#inbounds"

	sourceSrv, err := h.Store.ServerByID(u.ID, sourceServerID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}
	targetSrv, err := h.Store.ServerByID(u.ID, targetServerID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}

	srcXC, err := h.Agg.XuiClient(*sourceSrv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui (источник): "+err.Error(), back)
		return
	}
	inbounds, err := srcXC.ListInbounds(r.Context())
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось получить inbound'ы источника: "+err.Error(), back)
		return
	}
	var src *xui.RawInbound
	for i := range inbounds {
		if inbounds[i].ID == sourceInboundID {
			src = &inbounds[i]
			break
		}
	}
	if src == nil {
		h.flashErrAndRedirect(w, r, "Inbound не найден на исходном сервере", back)
		return
	}

	// Защита от очевидного конфликта при копии на тот же сервер.
	if sourceServerID == targetServerID {
		effectivePort := newPort
		if effectivePort == 0 {
			effectivePort = src.Port
		}
		if effectivePort == src.Port {
			h.flashErrAndRedirect(w, r, "Копия на тот же сервер требует другой порт", back)
			return
		}
	}

	cloned, err := cloneInbound(*src, newRemark, newPort)
	if err != nil {
		h.flashErrAndRedirect(w, r, "clone: "+err.Error(), back)
		return
	}

	tgtXC, err := h.Agg.XuiClient(*targetSrv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui (цель): "+err.Error(), back)
		return
	}
	if err := tgtXC.AddInbound(r.Context(), cloned); err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось создать inbound: "+err.Error(), back)
		return
	}
	h.Agg.RefreshNow(r.Context())
	h.setFlash(w, flashSuccess, fmt.Sprintf("Inbound скопирован: %q → :%d", cloned.Remark, cloned.Port))
	http.Redirect(w, r, "/dashboard/servers/"+strconv.FormatInt(targetServerID, 10)+"#inbounds", http.StatusSeeOther)
}

// cloneInbound создаёт копию inbound'а: сбрасывает id, счётчики трафика и tag,
// при необходимости меняет remark/port, генерирует новые UUID и переименовывает
// email каждого клиента в "subId-{remark}" (subId/flow/limits — сохраняются).
// Остальные поля settings (decryption, fallbacks и пр.) оставляются нетронутыми.
func cloneInbound(src xui.RawInbound, newRemark string, newPort int) (xui.RawInbound, error) {
	out := src
	out.ID = 0
	out.Up = 0
	out.Down = 0
	if newRemark != "" {
		out.Remark = newRemark
	}
	if newPort > 0 {
		out.Port = newPort
	}
	// Tag должен быть уникален в пределах панели; ставим конвенциональный
	// inbound-{port}, который 3x-ui генерирует сама.
	out.Tag = fmt.Sprintf("inbound-%d", out.Port)

	if src.Settings == "" {
		return out, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src.Settings), &raw); err != nil {
		return out, fmt.Errorf("parse settings: %w", err)
	}
	if rc, ok := raw["clients"]; ok {
		var clients []xui.InboundClient
		if err := json.Unmarshal(rc, &clients); err != nil {
			return out, fmt.Errorf("parse clients: %w", err)
		}
		for i := range clients {
			uuid, err := newClientUUID()
			if err != nil {
				return out, err
			}
			clients[i].ID = uuid
			// Переименование клиентов по конвенции subId+inbound name
			// (для уникальности email per-inbound и читаемости в панели).
			if clients[i].SubID != "" {
				clients[i].Email = defaultClientEmail(clients[i].SubID, out.Remark, out.ID)
			}
		}
		nc, err := json.Marshal(clients)
		if err != nil {
			return out, err
		}
		raw["clients"] = nc
		nb, err := json.Marshal(raw)
		if err != nil {
			return out, err
		}
		out.Settings = string(nb)
	}
	return out, nil
}

// POST /dashboard/inbounds/edit
// form: server_id, inbound_id, new_remark, new_port, enable (=1 если чекбокс)
//
// Тянет полный inbound, перетирает Remark/Port/Enable нужными значениями и
// отправляет UpdateInbound. Tag синхронизируется с (новым) портом.
func (h *Handler) inboundEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())
	serverID, _ := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, _ := strconv.Atoi(r.FormValue("inbound_id"))
	if serverID == 0 || inboundID == 0 {
		http.Error(w, "bad request: server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}
	newRemark := strings.TrimSpace(r.FormValue("new_remark"))
	newPort, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("new_port")))
	enable := r.FormValue("enable") == "1"

	back := "/dashboard/servers/" + strconv.FormatInt(serverID, 10) + "#inbounds"

	srv, err := h.Store.ServerByID(u.ID, serverID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}
	xc, err := h.Agg.XuiClient(*srv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui: "+err.Error(), back)
		return
	}
	inbounds, err := xc.ListInbounds(r.Context())
	if err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось получить inbound'ы: "+err.Error(), back)
		return
	}
	var src *xui.RawInbound
	for i := range inbounds {
		if inbounds[i].ID == inboundID {
			src = &inbounds[i]
			break
		}
	}
	if src == nil {
		h.flashErrAndRedirect(w, r, "Inbound не найден", back)
		return
	}
	updated := *src
	if newRemark != "" {
		updated.Remark = newRemark
	}
	if newPort > 0 && newPort != updated.Port {
		updated.Port = newPort
		updated.Tag = fmt.Sprintf("inbound-%d", updated.Port)
	}
	updated.Enable = enable

	if err := xc.UpdateInbound(r.Context(), inboundID, updated); err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось обновить inbound: "+err.Error(), back)
		return
	}
	h.Agg.RefreshNow(r.Context())
	h.setFlash(w, flashSuccess, fmt.Sprintf("Inbound %q обновлён", updated.Remark))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// POST /dashboard/inbounds/delete
// form: server_id, inbound_id
func (h *Handler) inboundDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := auth.FromContext(r.Context())
	serverID, _ := strconv.ParseInt(r.FormValue("server_id"), 10, 64)
	inboundID, _ := strconv.Atoi(r.FormValue("inbound_id"))
	if serverID == 0 || inboundID == 0 {
		http.Error(w, "bad request: server_id, inbound_id обязательны", http.StatusBadRequest)
		return
	}
	back := "/dashboard/servers/" + strconv.FormatInt(serverID, 10) + "#inbounds"

	srv, err := h.Store.ServerByID(u.ID, serverID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.flashErrAndRedirect(w, r, "Ошибка БД: "+err.Error(), back)
		return
	}
	xc, err := h.Agg.XuiClient(*srv)
	if err != nil {
		h.flashErrAndRedirect(w, r, "xui: "+err.Error(), back)
		return
	}
	if err := xc.DeleteInbound(r.Context(), inboundID); err != nil {
		h.flashErrAndRedirect(w, r, "Не удалось удалить inbound: "+err.Error(), back)
		return
	}
	h.Agg.RefreshNow(r.Context())
	h.setFlash(w, flashSuccess, "Inbound удалён со всеми клиентами")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

type serverFormData struct {
	ID                 int64
	Name               string
	APIURL             string
	Path               string
	Username           string
	HostOverride       string
	InsecureSkipVerify bool

	// Заполняется только в edit-режиме на GET — секция inbound'ов и список других
	// серверов для копирования.
	Inbounds     []serverEditInbound
	OtherServers []serverOption
	InboundsErr  string
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
	if r.Method == http.MethodGet {
		populateServerEditExtras(&form, h.Agg.Snapshot(), srv.ID, h.Store, u.ID)
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

// populateServerEditExtras наполняет форму списком inbound'ов сервера (из
// snapshot) и списком других серверов пользователя (для дропдауна копирования).
func populateServerEditExtras(form *serverFormData, snap *aggregator.Snapshot, serverID int64, store *storage.Store, userID int64) {
	for _, ssrv := range snap.Servers {
		if ssrv.ID != serverID {
			continue
		}
		if ssrv.Err != nil {
			form.InboundsErr = ssrv.Err.Error()
		}
		for _, ib := range ssrv.Inbounds {
			form.Inbounds = append(form.Inbounds, serverEditInbound{
				ID:         ib.ID,
				Remark:     ib.Remark,
				Port:       ib.Port,
				Network:    networkLabel(ib.Network),
				Security:   securityLabel(ib.Security),
				SecurityCl: strings.ToLower(securityLabel(ib.Security)),
				Enable:     ib.Enable,
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
	for _, s := range all {
		if s.ID == serverID {
			continue
		}
		form.OtherServers = append(form.OtherServers, serverOption{ID: s.ID, Name: s.Name})
	}
	sort.Slice(form.OtherServers, func(i, j int) bool {
		return strings.ToLower(form.OtherServers[i].Name) < strings.ToLower(form.OtherServers[j].Name)
	})
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
