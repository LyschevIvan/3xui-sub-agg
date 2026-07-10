package aggregator

import (
	"context"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

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
	State        ServerState
	FetchedAt    time.Time
	AttemptedAt  time.Time
	SyncErr      error // internal only; web UI maps State and never renders Error()
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

type serverClient struct {
	srv  storage.Server
	api  xui.PanelAPI
	host string
}

type panelFactory func(storage.Server) (xui.PanelAPI, error)

var (
	errServerTokenRequired      = errors.New("server API token is required")
	errServerTokenConfiguration = errors.New("server API token configuration is invalid")
)

type Aggregator struct {
	cfg     *config.Config
	store   *storage.Store
	snap    atomic.Pointer[Snapshot]
	trigger chan struct{}

	mu      sync.Mutex
	clients map[int64]*serverClient // id → кэш xui-клиента (чтобы не пересоздавать и не перелогиниваться каждый раз)
	links   *linkCache
	fetcher nativeFetcher

	panelFactory panelFactory
}

func New(cfg *config.Config, store *storage.Store) *Aggregator {
	links := newLinkCache(4)
	a := &Aggregator{
		cfg:          cfg,
		store:        store,
		trigger:      make(chan struct{}, 1),
		clients:      map[int64]*serverClient{},
		links:        links,
		fetcher:      nativeFetcher{links: links, workers: 4},
		panelFactory: defaultPanelFactory(cfg.RequestTimeout),
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

// normalizeHost accepts either a plain Host value or a URL. URL-form values
// retain an explicit port; plain values are only whitespace-trimmed.
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return s
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

// XuiClient is retained only for the legacy mutation handlers. Runtime reads
// use the token-only PanelAPI returned by clientFor.
func (a *Aggregator) XuiClient(srv storage.Server) (*xui.Client, error) {
	return xui.New(srv.APIURL, srv.Path, srv.Username, srv.Password, srv.InsecureSkipVerify, a.cfg.RequestTimeout)
}

func defaultPanelFactory(timeout time.Duration) panelFactory {
	return func(srv storage.Server) (xui.PanelAPI, error) {
		return xui.NewAPI(xui.APIConfig{
			BaseURL:            srv.APIURL,
			PanelPath:          srv.Path,
			Token:              srv.APIToken,
			InsecureSkipVerify: srv.InsecureSkipVerify,
			Timeout:            timeout,
		})
	}
}

// clientFor returns a cached token-only panel connection. Token storage
// failures are rejected before the factory can observe or construct anything.
func (a *Aggregator) clientFor(srv storage.Server) (*serverClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if srv.TokenError != nil {
		delete(a.clients, srv.ID)
		a.links.PruneServer(srv.ID, nil)
		return nil, fmtServerConfigurationError(srv.TokenError)
	}
	if srv.APIToken == "" {
		delete(a.clients, srv.ID)
		a.links.PruneServer(srv.ID, nil)
		return nil, errServerTokenRequired
	}
	if sc, ok := a.clients[srv.ID]; ok && sameConnection(sc.srv, srv) {
		return sc, nil
	}
	delete(a.clients, srv.ID)
	a.links.PruneServer(srv.ID, nil)
	api, err := a.panelFactory(srv)
	if err != nil {
		return nil, err
	}
	sc := &serverClient{srv: srv, api: api, host: publicHost(srv)}
	a.clients[srv.ID] = sc
	return sc, nil
}

func (a *Aggregator) purgeChangedConnection(srv storage.Server) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if cached, ok := a.clients[srv.ID]; ok && !sameConnection(cached.srv, srv) {
		delete(a.clients, srv.ID)
		a.links.PruneServer(srv.ID, nil)
	}
}

func fmtServerConfigurationError(err error) error {
	return errors.Join(errServerTokenConfiguration, err)
}

func sameConnection(a, b storage.Server) bool {
	return a.APIURL == b.APIURL && a.Path == b.Path &&
		a.APIToken == b.APIToken && a.InsecureSkipVerify == b.InsecureSkipVerify &&
		a.HostOverride == b.HostOverride
}

func publicHost(srv storage.Server) string {
	if h := normalizeHost(srv.HostOverride); h != "" {
		return h
	}
	if u, err := url.Parse(strings.TrimSpace(srv.APIURL)); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSpace(srv.APIURL)
}

func (a *Aggregator) refresh(ctx context.Context) {
	servers, err := a.store.ListAllServers()
	if err != nil {
		log.Printf("aggregator: list servers: %v", err)
		return
	}

	// Remove native clients and every link-cache entry for deleted servers.
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
	a.links.PruneDeletedServers(alive)

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
			attemptedAt := time.Now()
			sc, err := a.clientFor(srv)
			if err != nil {
				log.Printf("aggregator: server %q client init: %v", srv.Name, err)
				results[i] = failedServerSnapshot(prevByID, srv, ServerSnapshot{
					PublicHost: publicHost(srv), AttemptedAt: attemptedAt,
				}, serverStateForError(err), err)
				return
			}
			fetched, err := a.fetcher.Fetch(ctx, srv, sc.api, sc.host)
			if err != nil {
				log.Printf("aggregator: server %q fetch: %v", srv.Name, err)
				results[i] = failedServerSnapshot(prevByID, srv, fetched, serverStateForError(err), err)
				return
			}
			results[i] = fetched
		}(i, srv)
	}
	wg.Wait()

	a.snap.Store(&Snapshot{Servers: results, BuiltAt: time.Now()})
	log.Printf("aggregator: refreshed, servers=%d", len(results))
}

func failedServerSnapshot(
	prev map[int64]ServerSnapshot,
	srv storage.Server,
	attempt ServerSnapshot,
	state ServerState,
	err error,
) ServerSnapshot {
	attemptedAt := attempt.AttemptedAt
	if attemptedAt.IsZero() {
		attemptedAt = time.Now()
	}
	if old, ok := prev[srv.ID]; ok {
		old.ID = srv.ID
		old.UserID = srv.UserID
		old.Name = srv.Name
		old.PublicHost = attempt.PublicHost
		if old.PublicHost == "" {
			old.PublicHost = publicHost(srv)
		}
		if attempt.PanelVersion != "" {
			old.PanelVersion = attempt.PanelVersion
		}
		old.State = state
		old.AttemptedAt = attemptedAt
		old.SyncErr = err
		return old
	}
	return ServerSnapshot{
		ID:           srv.ID,
		UserID:       srv.UserID,
		Name:         srv.Name,
		PublicHost:   publicHost(srv),
		PanelVersion: attempt.PanelVersion,
		State:        state,
		AttemptedAt:  attemptedAt,
		SyncErr:      err,
	}
}

func serverStateForError(err error) ServerState {
	switch {
	case errors.Is(err, errServerTokenRequired):
		return ServerTokenRequired
	case errors.Is(err, errServerTokenConfiguration):
		return ServerConfigurationError
	case xui.IsKind(err, xui.ErrorUnauthorized):
		return ServerTokenRejected
	case xui.IsKind(err, xui.ErrorUnsupportedVersion):
		return ServerUnsupportedVersion
	default:
		return ServerUnavailable
	}
}
