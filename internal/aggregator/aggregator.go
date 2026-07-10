package aggregator

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/link"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

// ClientEntry — один «кусочек» для итоговой подписки.
// Ключом подписки служит SubID — клиент должен задать subId в 3x-ui;
// клиенты без subId в подписку не попадают.
type ClientEntry struct {
	ServerID      int64
	ServerName    string
	Email         string
	SubID         string
	ClientUUID    string // UUID клиента в 3x-ui — нужен для delete/update
	InboundID     int    // 3x-ui inbound id — для дедупа при отображении
	InboundRemark string // имя inbound'а, заданное в панели
	Port          int
	Network       string // tcp / ws / grpc / xhttp
	Security      string // none / tls / reality
	Link          string
	Enabled       bool
}

// ClientRef is one native 3x-ui client record. Multiple records may share the
// same SubID and must remain distinct.
type ClientRef struct {
	RecordID   *int
	Email      string
	SubID      string
	Enabled    bool
	InboundIDs []int
}

// ClientGroup contains every exact client record sharing a non-empty SubID.
type ClientGroup struct {
	SubID   string
	Records []ClientRef
}

// InboundInfo describes one native inbound, including protocols that cannot
// be rendered by the legacy subscription builder.
type InboundInfo struct {
	ID       int
	Remark   string
	Port     int
	Protocol string
	Network  string
	Security string
	Enable   bool
}

type ServerState string

const (
	ServerOK                 ServerState = "ok"
	ServerTokenRequired      ServerState = "token_required"
	ServerTokenRejected      ServerState = "token_rejected"
	ServerUnsupportedVersion ServerState = "unsupported_version"
	ServerUnavailable        ServerState = "panel_unavailable"
	ServerConfigurationError ServerState = "configuration_error"
	ServerDegraded           ServerState = "degraded"
)

// ServerSnapshot — состояние одного 3x-ui сервера.
type ServerSnapshot struct {
	ID           int64
	UserID       int64
	Name         string
	PublicHost   string
	PanelVersion string
	Inbounds     []InboundInfo
	Groups       map[string]ClientGroup
	Entries      []ClientEntry // transitional read compatibility; removed in Task 7
	State        ServerState
	FetchedAt    time.Time
	AttemptedAt  time.Time
	SyncErr      error // internal only; web UI maps State and never renders Error()
	Err          error // transitional template compatibility; removed in Task 7
}

// Snapshot — агрегированное состояние всех серверов.
type Snapshot struct {
	Servers []ServerSnapshot
	BuiltAt time.Time
}

// ByUser группирует серверы по user_id.
func (s *Snapshot) ByUser() map[int64][]ServerSnapshot {
	out := map[int64][]ServerSnapshot{}
	for _, srv := range s.Servers {
		out[srv.UserID] = append(out[srv.UserID], srv)
	}
	return out
}

// UserSubscriptions возвращает subId → []ClientEntry для серверов одного пользователя.
// Записи без subId пропускаются (они не образуют подписку).
func (s *Snapshot) UserSubscriptions(userID int64) map[string][]ClientEntry {
	m := map[string][]ClientEntry{}
	for _, srv := range s.Servers {
		if srv.UserID != userID {
			continue
		}
		for _, e := range srv.Entries {
			if !e.Enabled || e.SubID == "" {
				continue
			}
			m[e.SubID] = append(m[e.SubID], e)
		}
	}
	return m
}

type serverClient struct {
	srv    storage.Server
	client *xui.Client
	host   string
}

type Aggregator struct {
	cfg     *config.Config
	store   *storage.Store
	snap    atomic.Pointer[Snapshot]
	trigger chan struct{}

	mu      sync.Mutex
	clients map[int64]*serverClient // id → кэш xui-клиента (чтобы не пересоздавать и не перелогиниваться каждый раз)
}

func New(cfg *config.Config, store *storage.Store) *Aggregator {
	a := &Aggregator{
		cfg:     cfg,
		store:   store,
		trigger: make(chan struct{}, 1),
		clients: map[int64]*serverClient{},
	}
	a.snap.Store(&Snapshot{BuiltAt: time.Now()})
	return a
}

func (a *Aggregator) Snapshot() *Snapshot { return a.snap.Load() }

// Trigger просит агрегатор обновиться вне графика (неблокирующе).
func (a *Aggregator) Trigger() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// RefreshNow запускает обновление snapshot синхронно — для использования после
// UI-мутаций, чтобы ответ сразу отражал новое состояние без ожидания тикера.
// Опрашивает все сервера пользователя; на ~5 серверах занимает ~1–2 секунды.
func (a *Aggregator) RefreshNow(ctx context.Context) {
	a.refresh(ctx)
}

// normalizeHost принимает либо чистый хост, либо URL, возвращает голый hostname.
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return strings.TrimSuffix(s, "/")
}

// Run блокируется до отмены контекста, периодически обновляя snapshot.
func (a *Aggregator) Run(ctx context.Context) {
	a.refresh(ctx)
	t := time.NewTicker(a.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh(ctx)
		case <-a.trigger:
			a.refresh(ctx)
		}
	}
}

// XuiClient возвращает кэшированный xui-клиент для сервера. Используется
// внешними обработчиками (UI-мутации) — переиспользует ту же сессию, что и
// фоновый refresh, чтобы не плодить логины.
func (a *Aggregator) XuiClient(srv storage.Server) (*xui.Client, error) {
	sc, err := a.clientFor(srv)
	if err != nil {
		return nil, err
	}
	return sc.client, nil
}

// clientFor возвращает кэшированный или вновь созданный xui-клиент для сервера.
// Если параметры подключения изменились — пересоздаёт.
func (a *Aggregator) clientFor(srv storage.Server) (*serverClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if sc, ok := a.clients[srv.ID]; ok && sameConnection(sc.srv, srv) {
		sc.srv = srv // имя / host_override могли смениться
		sc.host = publicHost(srv)
		return sc, nil
	}
	c, err := xui.New(srv.APIURL, srv.Path, srv.Username, srv.Password, srv.InsecureSkipVerify, a.cfg.RequestTimeout)
	if err != nil {
		return nil, err
	}
	sc := &serverClient{srv: srv, client: c, host: publicHost(srv)}
	a.clients[srv.ID] = sc
	return sc, nil
}

func sameConnection(a, b storage.Server) bool {
	return a.APIURL == b.APIURL && a.Path == b.Path &&
		a.Username == b.Username && a.Password == b.Password &&
		a.InsecureSkipVerify == b.InsecureSkipVerify
}

func publicHost(srv storage.Server) string {
	if h := normalizeHost(srv.HostOverride); h != "" {
		return h
	}
	if u, err := url.Parse(srv.APIURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return srv.APIURL
}

func (a *Aggregator) refresh(ctx context.Context) {
	servers, err := a.store.ListAllServers()
	if err != nil {
		log.Printf("aggregator: list servers: %v", err)
		return
	}

	// Удаляем кэш клиентов для серверов, которых больше нет в БД.
	a.mu.Lock()
	alive := map[int64]struct{}{}
	for _, s := range servers {
		alive[s.ID] = struct{}{}
	}
	for id := range a.clients {
		if _, ok := alive[id]; !ok {
			delete(a.clients, id)
		}
	}
	a.mu.Unlock()

	prev := a.snap.Load()
	prevByID := map[int64]ServerSnapshot{}
	if prev != nil {
		for _, s := range prev.Servers {
			prevByID[s.ID] = s
		}
	}

	results := make([]ServerSnapshot, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, srv storage.Server) {
			defer wg.Done()
			sc, err := a.clientFor(srv)
			if err != nil {
				log.Printf("aggregator: server %q client init: %v", srv.Name, err)
				results[i] = fallback(prevByID, srv, err)
				return
			}
			infos, entries, err := a.fetchServer(ctx, sc)
			if err != nil {
				log.Printf("aggregator: server %q fetch: %v", srv.Name, err)
				results[i] = fallback(prevByID, srv, err)
				return
			}
			results[i] = ServerSnapshot{
				ID:         srv.ID,
				UserID:     srv.UserID,
				Name:       srv.Name,
				PublicHost: sc.host,
				Inbounds:   infos,
				Entries:    entries,
				FetchedAt:  time.Now(),
			}
		}(i, srv)
	}
	wg.Wait()

	a.snap.Store(&Snapshot{Servers: results, BuiltAt: time.Now()})
	log.Printf("aggregator: refreshed, servers=%d", len(results))
}

func fallback(prev map[int64]ServerSnapshot, srv storage.Server, err error) ServerSnapshot {
	if old, ok := prev[srv.ID]; ok {
		old.Err = err
		return old
	}
	return ServerSnapshot{
		ID:         srv.ID,
		UserID:     srv.UserID,
		Name:       srv.Name,
		PublicHost: publicHost(srv),
		FetchedAt:  time.Now(),
		Err:        err,
	}
}

func (a *Aggregator) fetchServer(ctx context.Context, sc *serverClient) ([]InboundInfo, []ClientEntry, error) {
	inbounds, err := sc.client.ListInbounds(ctx)
	if err != nil {
		return nil, nil, err
	}
	var infos []InboundInfo
	var out []ClientEntry
	for _, ib := range inbounds {
		if ib.Protocol != "vless" {
			continue
		}
		ss, err := xui.ParseStream(ib.StreamSettings)
		if err != nil {
			log.Printf("aggregator: %s inbound %d streamSettings parse: %v", sc.srv.Name, ib.ID, err)
			continue
		}
		infos = append(infos, InboundInfo{
			ID:       ib.ID,
			Remark:   ib.Remark,
			Port:     ib.Port,
			Network:  ss.Network,
			Security: ss.Security,
			Enable:   ib.Enable,
		})
		clients, err := xui.ParseClients(ib.Settings)
		if err != nil {
			log.Printf("aggregator: %s inbound %d settings parse: %v", sc.srv.Name, ib.ID, err)
			continue
		}
		for _, c := range clients {
			remark := fmt.Sprintf("%s — %s", sc.srv.Name, ib.Remark)
			if ib.Remark == "" {
				remark = sc.srv.Name
			}
			ln, err := link.BuildVless(sc.host, ib.Port, remark, c, ss)
			if err != nil {
				log.Printf("aggregator: %s inbound %d build link: %v", sc.srv.Name, ib.ID, err)
				continue
			}
			out = append(out, ClientEntry{
				ServerID:      sc.srv.ID,
				ServerName:    sc.srv.Name,
				Email:         c.Email,
				SubID:         c.SubID,
				ClientUUID:    c.ID,
				InboundID:     ib.ID,
				InboundRemark: ib.Remark,
				Port:          ib.Port,
				Network:       ss.Network,
				Security:      ss.Security,
				Link:          ln,
				Enabled:       ib.Enable && c.Enable,
			})
		}
	}
	return infos, out, nil
}
