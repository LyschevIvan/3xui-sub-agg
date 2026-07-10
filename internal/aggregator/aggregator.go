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
	Epoch        uint64
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
	srv    storage.Server
	api    xui.PanelAPI
	host   string
	epoch  uint64
	ctx    context.Context
	cancel context.CancelFunc
}

type panelFactory func(storage.Server) (xui.PanelAPI, error)

type observedServer struct {
	exists bool
	srv    storage.Server
}

var (
	errServerTokenRequired      = errors.New("server API token is required")
	errServerTokenConfiguration = errors.New("server API token configuration is invalid")
	errStaleConnection          = errors.New("native connection changed")
)

type Aggregator struct {
	cfg     *config.Config
	store   *storage.Store
	snap    atomic.Pointer[Snapshot]
	trigger chan struct{}

	mu          sync.Mutex
	clients     map[int64]*serverClient // id → кэш xui-клиента (чтобы не пересоздавать и не перелогиниваться каждый раз)
	epochs      map[int64]uint64
	observed    map[int64]observedServer
	discoveries map[discoveryKey]*discoveryFlight
	links       *linkCache
	fetcher     nativeFetcher
	refreshMu   sync.Mutex

	panelFactory panelFactory
}

func New(cfg *config.Config, store *storage.Store) *Aggregator {
	links := newLinkCache(4)
	a := &Aggregator{
		cfg:          cfg,
		store:        store,
		trigger:      make(chan struct{}, 1),
		clients:      map[int64]*serverClient{},
		epochs:       map[int64]uint64{},
		observed:     map[int64]observedServer{},
		discoveries:  map[discoveryKey]*discoveryFlight{},
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

	authoritative, err := a.authoritativeServerLocked(srv)
	if err != nil {
		return nil, err
	}
	if authoritative.TokenError != nil {
		return nil, fmtServerConfigurationError(authoritative.TokenError)
	}
	if authoritative.APIToken == "" {
		return nil, errServerTokenRequired
	}
	if sc, ok := a.clients[authoritative.ID]; ok && sameConnection(sc.srv, authoritative) && sc.ctx.Err() == nil {
		return sc, nil
	}
	if _, ok := a.clients[authoritative.ID]; ok {
		a.invalidateServerLocked(authoritative.ID)
	}
	epoch := a.epochs[authoritative.ID]
	connectionCtx, cancel := context.WithCancel(context.Background())
	api, err := a.panelFactory(authoritative)
	if err != nil {
		cancel()
		return nil, err
	}
	sc := &serverClient{
		srv: authoritative, api: api, host: publicHost(authoritative), epoch: epoch,
		ctx: connectionCtx, cancel: cancel,
	}
	a.clients[authoritative.ID] = sc
	return sc, nil
}

// authoritativeServer serializes the owned row lookup with revision/epoch
// observation. Storage never calls back into Aggregator, so this lock order has
// no inverse; the lock is released before any panel operation.
func (a *Aggregator) authoritativeServer(expected storage.Server) (storage.Server, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authoritativeServerLocked(expected)
}

func (a *Aggregator) authoritativeServerLocked(expected storage.Server) (storage.Server, error) {
	authoritative, err := a.store.ServerByID(expected.UserID, expected.ID)
	if errors.Is(err, storage.ErrNotFound) {
		a.observeServerLocked(expected.ID, nil)
		return storage.Server{}, errStaleConnection
	}
	if err != nil {
		return storage.Server{}, err
	}
	a.observeServerLocked(authoritative.ID, authoritative)
	if !sameServerRevision(expected, *authoritative) {
		return storage.Server{}, errStaleConnection
	}
	return *authoritative, nil
}

func (a *Aggregator) invalidateServerLocked(serverID int64) {
	if cached, ok := a.clients[serverID]; ok {
		cached.cancel()
		delete(a.clients, serverID)
	}
	a.epochs[serverID]++
	a.links.PruneServer(serverID, nil)
}

func (a *Aggregator) observeServerLocked(serverID int64, srv *storage.Server) {
	next := observedServer{}
	if srv != nil {
		next = observedServer{exists: true, srv: *srv}
	}
	previous, observed := a.observed[serverID]
	if !observed {
		a.observed[serverID] = next
		if a.epochs[serverID] == 0 {
			a.epochs[serverID] = 1
		}
		return
	}
	if sameObservedServer(previous, next) {
		return
	}
	a.invalidateServerLocked(serverID)
	a.observed[serverID] = next
}

func sameObservedServer(a, b observedServer) bool {
	if a.exists != b.exists {
		return false
	}
	if !a.exists {
		return true
	}
	return sameServerRevision(a.srv, b.srv)
}

func sameServerRevision(a, b storage.Server) bool {
	return a.ID == b.ID && a.UserID == b.UserID &&
		a.HasAPIToken == b.HasAPIToken && (a.TokenError != nil) == (b.TokenError != nil) &&
		sameConnection(a, b)
}

func (a *Aggregator) currentClient(sc *serverClient) bool {
	if sc == nil || sc.ctx.Err() != nil {
		return false
	}
	a.mu.Lock()
	current := a.clients[sc.srv.ID]
	ok := current == sc && current.epoch == sc.epoch && current.ctx.Err() == nil
	a.mu.Unlock()
	return ok
}

func (a *Aggregator) cachedEpoch(srv storage.Server) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cached, ok := a.clients[srv.ID]; ok && sameConnection(cached.srv, srv) && cached.ctx.Err() == nil {
		return cached.epoch
	}
	return 0
}

func (a *Aggregator) serverEpoch(serverID int64) uint64 {
	a.mu.Lock()
	epoch := a.epochs[serverID]
	a.mu.Unlock()
	return epoch
}

func (a *Aggregator) connectionContext(parent context.Context, sc *serverClient) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(sc.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (a *Aggregator) connectionCallContext(parent context.Context, sc *serverClient) (context.Context, context.CancelFunc) {
	ctx, cancel := a.callContext(parent)
	stop := context.AfterFunc(sc.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
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

type connectionFetchAPI struct {
	agg *Aggregator
	sc  *serverClient
}

func (c connectionFetchAPI) Validate(ctx context.Context) (xui.ServerStatus, error) {
	if !c.agg.currentClient(c.sc) {
		return xui.ServerStatus{}, errStaleConnection
	}
	callCtx, cancel := c.agg.connectionCallContext(ctx, c.sc)
	defer cancel()
	status, err := c.sc.api.Validate(callCtx)
	if !c.agg.currentClient(c.sc) {
		return xui.ServerStatus{}, errStaleConnection
	}
	return status, err
}

func (c connectionFetchAPI) ListClients(ctx context.Context) ([]xui.ClientSummary, error) {
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	callCtx, cancel := c.agg.connectionCallContext(ctx, c.sc)
	defer cancel()
	clients, err := c.sc.api.ListClients(callCtx)
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	return clients, err
}

func (c connectionFetchAPI) ListSlimInbounds(ctx context.Context) ([]xui.InboundSummary, error) {
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	callCtx, cancel := c.agg.connectionCallContext(ctx, c.sc)
	defer cancel()
	inbounds, err := c.sc.api.ListSlimInbounds(callCtx)
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	return inbounds, err
}

func (c connectionFetchAPI) SubLinks(ctx context.Context, subID, host string) ([]string, error) {
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	callCtx, cancel := c.agg.connectionCallContext(ctx, c.sc)
	defer cancel()
	links, err := c.sc.api.SubLinks(callCtx, subID, host)
	if !c.agg.currentClient(c.sc) {
		return nil, errStaleConnection
	}
	return links, err
}

func (a *Aggregator) reconcileConnections(servers []storage.Server) {
	alive := make(map[int64]struct{}, len(servers))
	a.mu.Lock()
	checked := make(map[int64]struct{}, len(servers))
	for _, preloaded := range servers {
		checked[preloaded.ID] = struct{}{}
		a.reconcileServerLocked(preloaded.ID, preloaded.UserID, alive)
	}
	for id, previous := range a.observed {
		if _, ok := checked[id]; ok || !previous.exists {
			continue
		}
		a.reconcileServerLocked(id, previous.srv.UserID, alive)
	}
	a.mu.Unlock()
	a.links.PruneDeletedServers(alive)
}

func (a *Aggregator) reconcileServerLocked(serverID, userID int64, alive map[int64]struct{}) {
	authoritative, err := a.store.ServerByID(userID, serverID)
	switch {
	case err == nil:
		alive[serverID] = struct{}{}
		a.observeServerLocked(serverID, authoritative)
	case errors.Is(err, storage.ErrNotFound):
		a.observeServerLocked(serverID, nil)
	default:
		// A storage read failure is not proof of deletion. Preserve current
		// connection/cache state and let the next reconciliation retry.
		alive[serverID] = struct{}{}
	}
}

func (a *Aggregator) refresh(ctx context.Context) {
	servers, err := a.store.ListAllServers()
	if err != nil {
		log.Printf("aggregator: list servers failed")
		return
	}
	a.reconcileConnections(servers)

	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	// A refresh may have waited behind an older attempt. Re-read storage and
	// reconcile again so its connections match the configuration it publishes.
	servers, err = a.store.ListAllServers()
	if err != nil {
		log.Printf("aggregator: list servers failed")
		return
	}
	a.reconcileConnections(servers)

	prev := a.snap.Load()
	prevByID := map[int64]ServerSnapshot{}
	if prev != nil {
		for _, s := range prev.Servers {
			prevByID[s.ID] = s
		}
	}

	results := make([]ServerSnapshot, len(servers))
	resultClients := make([]*serverClient, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, srv storage.Server) {
			defer wg.Done()
			attemptedAt := time.Now()
			sc, err := a.clientFor(srv)
			if err != nil {
				log.Printf("aggregator: server %q connection unavailable", srv.Name)
				results[i] = failedServerSnapshot(prevByID, srv, ServerSnapshot{
					PublicHost: publicHost(srv), AttemptedAt: attemptedAt,
				}, serverStateForError(err), err)
				return
			}
			resultClients[i] = sc
			connectionCtx, cancel := a.connectionContext(ctx, sc)
			fetched, err := a.fetcher.FetchEpoch(
				connectionCtx, srv, connectionFetchAPI{agg: a, sc: sc}, sc.host, sc.epoch,
			)
			cancel()
			fetched.Epoch = sc.epoch
			if !a.currentClient(sc) {
				err = errStaleConnection
			}
			if err != nil {
				log.Printf("aggregator: server %q refresh failed", srv.Name)
				results[i] = failedServerSnapshot(prevByID, srv, fetched, serverStateForError(err), err)
				return
			}
			results[i] = fetched
		}(i, srv)
	}
	wg.Wait()

	// Publication and connection invalidation share the same mutex. A result
	// can only become visible after the row and exact connection epoch are
	// authoritatively revalidated at this linearization point.
	a.mu.Lock()
	published := make([]ServerSnapshot, 0, len(results))
	for i, preloaded := range servers {
		sc := resultClients[i]
		authoritative, lookupErr := a.store.ServerByID(preloaded.UserID, preloaded.ID)
		if errors.Is(lookupErr, storage.ErrNotFound) {
			a.observeServerLocked(preloaded.ID, nil)
			continue
		}
		if lookupErr != nil {
			// A storage error is not proof of deletion. Preserve a safe row,
			// while still rejecting an in-memory connection that is no longer current.
			if sc != nil {
				if current := a.clients[sc.srv.ID]; current != sc || current.epoch != sc.epoch || current.ctx.Err() != nil {
					published = append(published, failedServerSnapshot(prevByID, preloaded, ServerSnapshot{
						Epoch: a.epochs[preloaded.ID], PublicHost: publicHost(preloaded), AttemptedAt: time.Now(),
					}, ServerUnavailable, errStaleConnection))
					continue
				}
			}
			published = append(published, results[i])
			continue
		}

		revisionChanged := !sameServerRevision(preloaded, *authoritative)
		a.observeServerLocked(authoritative.ID, authoritative)
		currentConnection := true
		if sc != nil {
			current := a.clients[sc.srv.ID]
			currentConnection = current == sc && current.epoch == sc.epoch && current.ctx.Err() == nil
		}
		if revisionChanged || !currentConnection {
			published = append(published, failedServerSnapshot(prevByID, *authoritative, ServerSnapshot{
				Epoch: a.epochs[authoritative.ID], PublicHost: publicHost(*authoritative), AttemptedAt: time.Now(),
			}, ServerUnavailable, errStaleConnection))
			continue
		}
		result := results[i]
		result.ID = authoritative.ID
		result.UserID = authoritative.UserID
		result.Name = authoritative.Name
		result.PublicHost = publicHost(*authoritative)
		published = append(published, result)
	}
	a.snap.Store(&Snapshot{Servers: published, BuiltAt: time.Now()})
	a.mu.Unlock()
	log.Printf("aggregator: refreshed, servers=%d", len(published))
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
		Epoch:        attempt.Epoch,
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
