package aggregator

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type fakePanel struct {
	mu sync.Mutex

	status      xui.ServerStatus
	validateErr error
	clients     []xui.ClientSummary
	clientsErr  error
	clientsFn   func(context.Context) ([]xui.ClientSummary, error)
	inbounds    []xui.InboundSummary
	inboundsErr error
	links       map[string][]string
	linkErrs    map[string]error
	linksFn     func(context.Context, string, string) ([]string, error)

	validateCalls int
	clientCalls   int
	linkCalls     int
}

func (f *fakePanel) Validate(context.Context) (xui.ServerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validateCalls++
	return f.status, f.validateErr
}

func (f *fakePanel) ListClients(ctx context.Context) ([]xui.ClientSummary, error) {
	f.mu.Lock()
	f.clientCalls++
	fn := f.clientsFn
	clients := append([]xui.ClientSummary(nil), f.clients...)
	err := f.clientsErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return clients, err
}

func (f *fakePanel) ListSlimInbounds(context.Context) ([]xui.InboundSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]xui.InboundSummary(nil), f.inbounds...), f.inboundsErr
}

func (f *fakePanel) SubLinks(ctx context.Context, subID, host string) ([]string, error) {
	f.mu.Lock()
	f.linkCalls++
	fn := f.linksFn
	links := append([]string(nil), f.links[subID]...)
	err := f.linkErrs[subID]
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, subID, host)
	}
	return links, err
}

func (*fakePanel) GetClient(context.Context, string) (xui.ClientDetail, error) {
	return xui.ClientDetail{}, nil
}
func (*fakePanel) AddClient(context.Context, xui.ClientPayload, []int) error     { return nil }
func (*fakePanel) UpdateClient(context.Context, string, xui.ClientPayload) error { return nil }
func (*fakePanel) DeleteClient(context.Context, string) error                    { return nil }
func (*fakePanel) AttachClient(context.Context, string, []int) error             { return nil }
func (*fakePanel) DetachClient(context.Context, string, []int) error             { return nil }
func (*fakePanel) GetInbound(context.Context, int) (xui.InboundDocument, error)  { return nil, nil }
func (*fakePanel) AddInbound(context.Context, xui.InboundDocument) (xui.InboundDocument, error) {
	return nil, nil
}
func (*fakePanel) UpdateInbound(context.Context, int, xui.InboundDocument) (xui.InboundDocument, error) {
	return nil, nil
}
func (*fakePanel) DeleteInbound(context.Context, int) error { return nil }

func activePanel(subID, link string) *fakePanel {
	return &fakePanel{
		status:   xui.ServerStatus{PanelVersion: "3.4.2"},
		clients:  []xui.ClientSummary{{Email: subID + "@example", SubID: subID, Enable: true, InboundIDs: []int{1}}},
		inbounds: []xui.InboundSummary{{ID: 1, Remark: "main", Enable: true, Port: 443, Protocol: "vless"}},
		links:    map[string][]string{subID: {link}},
	}
}

func testStore(t *testing.T) (*storage.Store, *storage.User, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "data.db")
	store, err := storage.Open(dbPath, secrets.New("task-7-test-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	user, err := store.CreateUser("owner", "unused", false)
	if err != nil {
		t.Fatal(err)
	}
	return store, user, dbPath
}

func createServer(t *testing.T, store *storage.Store, userID int64, name, token string) storage.Server {
	t.Helper()
	created, err := store.CreateServer(&storage.Server{
		UserID: userID, Name: name, APIURL: "https://panel.example:9443", Path: "/admin/",
		APIToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return *created
}

func newTestAggregator(store *storage.Store, panels map[int64]*fakePanel, calls *int) *Aggregator {
	a := New(&config.Config{RequestTimeout: 250 * time.Millisecond, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if calls != nil {
			*calls++
		}
		panel, ok := panels[srv.ID]
		if !ok {
			return nil, errors.New("unexpected server")
		}
		return panel, nil
	}
	return a
}

func TestRefreshUsesNativeTokenPanelsAndEffectiveHost(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "native-token")
	srv.HostOverride = " https://public.example:8443/path "
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}

	panel := activePanel("group", "vless://from-panel")
	var factoryCalls int
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, &factoryCalls)
	a.RefreshNow(context.Background())

	got := a.Snapshot().Servers
	if factoryCalls != 1 || len(got) != 1 {
		t.Fatalf("factoryCalls=%d snapshot=%+v", factoryCalls, got)
	}
	if got[0].PublicHost != "public.example:8443" || got[0].PanelVersion != "3.4.2" || got[0].State != ServerOK {
		t.Fatalf("snapshot=%+v", got[0])
	}
	if _, ok := got[0].Groups["group"]; !ok || got[0].FetchedAt.IsZero() || got[0].AttemptedAt.IsZero() {
		t.Fatalf("native inventory missing: %+v", got[0])
	}
	value, ok := a.links.Get(linkKey{
		ServerID: srv.ID, Epoch: a.cachedEpoch(srv),
		SubID: "group", EffectiveHost: "public.example:8443",
	})
	if !ok || !slices.Equal(value.Links, []string{"vless://from-panel"}) {
		t.Fatalf("native links=%+v ok=%v", value, ok)
	}
}

func TestRefreshShortCircuitsMissingAndUnreadableTokensPerServer(t *testing.T) {
	store, user, dbPath := testStore(t)
	missing := createServer(t, store, user.ID, "missing", "")
	broken := createServer(t, store, user.ID, "broken", "will-corrupt")
	good := createServer(t, store, user.ID, "good", "working-token")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE servers SET api_token = 'plaintext-corruption' WHERE id = ?`, broken.ID); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_ = raw.Close()

	var factoryCalls int
	a := newTestAggregator(store, map[int64]*fakePanel{good.ID: activePanel("ok", "vless://ok")}, &factoryCalls)
	a.RefreshNow(context.Background())

	states := map[int64]ServerState{}
	for _, snapshot := range a.Snapshot().Servers {
		states[snapshot.ID] = snapshot.State
	}
	if factoryCalls != 1 || states[missing.ID] != ServerTokenRequired || states[broken.ID] != ServerConfigurationError || states[good.ID] != ServerOK {
		t.Fatalf("factoryCalls=%d states=%v", factoryCalls, states)
	}
}

func TestRefreshMapsPanelErrorsAndRetainsOnlyFailingInventory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state ServerState
	}{
		{"unauthorized", &xui.Error{Kind: xui.ErrorUnauthorized}, ServerTokenRejected},
		{"unsupported", &xui.Error{Kind: xui.ErrorUnsupportedVersion}, ServerUnsupportedVersion},
		{"transport", &xui.Error{Kind: xui.ErrorTransport}, ServerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, user, _ := testStore(t)
			failedSrv := createServer(t, store, user.ID, "failed", "token-a")
			goodSrv := createServer(t, store, user.ID, "good", "token-b")
			failedPanel := activePanel("old", "vless://old")
			goodPanel := activePanel("other", "vless://other")
			a := newTestAggregator(store, map[int64]*fakePanel{failedSrv.ID: failedPanel, goodSrv.ID: goodPanel}, nil)
			a.RefreshNow(context.Background())

			before := a.Snapshot()
			var old ServerSnapshot
			for _, snapshot := range before.Servers {
				if snapshot.ID == failedSrv.ID {
					old = snapshot
				}
			}
			time.Sleep(time.Millisecond)
			failedPanel.mu.Lock()
			failedPanel.validateErr = tc.err
			failedPanel.clients = nil
			failedPanel.inbounds = nil
			failedPanel.mu.Unlock()
			goodPanel.mu.Lock()
			goodPanel.clients = []xui.ClientSummary{{Email: "new", SubID: "new", Enable: true, InboundIDs: []int{1}}}
			goodPanel.links = map[string][]string{"new": {"vmess://new"}}
			goodPanel.mu.Unlock()

			a.RefreshNow(context.Background())
			var failed, good ServerSnapshot
			for _, snapshot := range a.Snapshot().Servers {
				if snapshot.ID == failedSrv.ID {
					failed = snapshot
				} else if snapshot.ID == goodSrv.ID {
					good = snapshot
				}
			}
			if failed.State != tc.state || failed.SyncErr == nil || !failed.FetchedAt.Equal(old.FetchedAt) || !failed.AttemptedAt.After(old.AttemptedAt) {
				t.Fatalf("failed=%+v old=%+v", failed, old)
			}
			if _, ok := failed.Groups["old"]; !ok {
				t.Fatalf("failed server inventory not retained: %+v", failed.Groups)
			}
			if _, ok := good.Groups["new"]; !ok || good.State != ServerOK {
				t.Fatalf("other server did not refresh: %+v", good)
			}
		})
	}
}

func TestConnectionChangesAndDeletionPurgeOnlyThatServersLinks(t *testing.T) {
	store, user, _ := testStore(t)
	first := createServer(t, store, user.ID, "first", "token-a")
	second := createServer(t, store, user.ID, "second", "token-b")
	a := newTestAggregator(store, map[int64]*fakePanel{first.ID: activePanel("a", "vless://a"), second.ID: activePanel("b", "vless://b")}, nil)

	oldFirst := linkKey{ServerID: first.ID, SubID: "cached", EffectiveHost: "old-host"}
	oldSecond := linkKey{ServerID: second.ID, SubID: "cached", EffectiveHost: "old-host"}
	mustRefreshLinkCache(t, a.links, oldFirst, []string{"vless://first"})
	mustRefreshLinkCache(t, a.links, oldSecond, []string{"vless://second"})
	if _, err := a.clientFor(first); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.HostOverride = "changed.example:7443"
	if err := store.UpdateServer(&changed); err != nil {
		t.Fatal(err)
	}
	if _, err := a.clientFor(changed); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.links.Get(oldFirst); ok {
		t.Fatal("changed server cache was retained")
	}
	if _, ok := a.links.Get(oldSecond); !ok {
		t.Fatal("other server cache was purged")
	}

	if err := store.DeleteServer(user.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	a.RefreshNow(context.Background())
	if _, ok := a.links.Get(oldSecond); ok {
		t.Fatal("deleted server cache was retained")
	}
}

func TestConnectionIdentityAndEffectiveHost(t *testing.T) {
	base := storage.Server{APIURL: "https://panel.example:9443/root", Path: "/admin/", APIToken: "token", InsecureSkipVerify: true, HostOverride: "edge.example:8443"}
	for _, mutate := range []func(*storage.Server){
		func(s *storage.Server) { s.APIURL += "/changed" },
		func(s *storage.Server) { s.Path = "/other/" },
		func(s *storage.Server) { s.APIToken = "other-token" },
		func(s *storage.Server) { s.InsecureSkipVerify = false },
		func(s *storage.Server) { s.HostOverride = "other.example" },
	} {
		changed := base
		mutate(&changed)
		if sameConnection(base, changed) {
			t.Fatalf("connection change ignored: base=%+v changed=%+v", base, changed)
		}
	}
	if got := publicHost(base); got != "edge.example:8443" {
		t.Fatalf("plain override=%q", got)
	}
	base.HostOverride = " https://public.example:7443/some/path "
	if got := publicHost(base); got != "public.example:7443" {
		t.Fatalf("URL override=%q", got)
	}
	base.HostOverride = ""
	if got := publicHost(base); got != "panel.example:9443" {
		t.Fatalf("API URL fallback=%q", got)
	}
	if strings.Contains(publicHost(base), "root") {
		t.Fatalf("path leaked into host: %q", publicHost(base))
	}
}

func completedSnapshot(srv storage.Server, groups ...string) ServerSnapshot {
	groupMap := make(map[string]ClientGroup, len(groups))
	for _, subID := range groups {
		groupMap[subID] = ClientGroup{SubID: subID, Records: []ClientRef{{Email: subID + "@example", SubID: subID, Enabled: true, InboundIDs: []int{1}}}}
	}
	return ServerSnapshot{
		ID: srv.ID, UserID: srv.UserID, Name: srv.Name, PublicHost: publicHost(srv),
		Groups: groupMap, State: ServerOK, FetchedAt: time.Now(), AttemptedAt: time.Now(),
	}
}

func TestResolveSubscriptionUsesSnapshotCacheWithoutPanelCalls(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("exact", "vless://should-not-fetch")
	var factoryCalls int
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, &factoryCalls)
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "exact")}, BuiltAt: time.Now()})
	mustRefreshLinkCache(t, a.links, linkKey{ServerID: srv.ID, SubID: "exact", EffectiveHost: publicHost(srv)}, []string{"vmess://cached"})

	result, err := a.ResolveSubscription(context.Background(), user.ID, "exact")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Links, []string{"vmess://cached"}) || result.Partial {
		t.Fatalf("result=%+v", result)
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if factoryCalls != 0 || panel.clientCalls != 0 || panel.linkCalls != 0 {
		t.Fatalf("factory=%d clients=%d links=%d", factoryCalls, panel.clientCalls, panel.linkCalls)
	}
}

func TestResolveSubscriptionMarksCachedLinksPartialForFailedRelevantServer(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("exact", "vless://should-not-fetch")
	var factoryCalls int
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, &factoryCalls)
	snapshot := completedSnapshot(srv, "exact")
	snapshot.State = ServerUnavailable
	snapshot.SyncErr = errors.New("last refresh failed")
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{snapshot}, BuiltAt: time.Now()})
	mustRefreshLinkCache(t, a.links, linkKey{ServerID: srv.ID, SubID: "exact", EffectiveHost: publicHost(srv)}, []string{"vmess://stale"})

	result, err := a.ResolveSubscription(context.Background(), user.ID, "exact")
	if err != nil || !slices.Equal(result.Links, []string{"vmess://stale"}) || !result.Partial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls=%d", factoryCalls)
	}
}

func TestResolveSubscriptionMarksUninitializedCachedLinksPartialForFailedServer(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("exact", "vless://should-not-fetch")
	var factoryCalls int
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, &factoryCalls)
	snapshot := completedSnapshot(srv, "exact")
	snapshot.FetchedAt = time.Time{}
	snapshot.State = ServerConfigurationError
	snapshot.SyncErr = errors.New("configuration detail")
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{snapshot}, BuiltAt: time.Now()})
	mustRefreshLinkCache(t, a.links, linkKey{ServerID: srv.ID, SubID: "exact", EffectiveHost: publicHost(srv)}, []string{"vmess://stale"})

	result, err := a.ResolveSubscription(context.Background(), user.ID, "exact")
	if err != nil || !slices.Equal(result.Links, []string{"vmess://stale"}) || !result.Partial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls=%d", factoryCalls)
	}
}

func TestResolveSubscriptionFencesOldDiscoveryAfterConnectionChange(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "old-token")
	srv.HostOverride = "old.example:443"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}

	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	oldPanel := activePanel("group", "vless://old-connection")
	oldClients := append([]xui.ClientSummary(nil), oldPanel.clients...)
	oldPanel.clientsFn = func(context.Context) ([]xui.ClientSummary, error) {
		close(oldStarted)
		<-releaseOld
		return oldClients, nil
	}
	newPanel := activePanel("group", "vless://new-connection")

	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(candidate storage.Server) (xui.PanelAPI, error) {
		if candidate.APIToken == "old-token" {
			return oldPanel, nil
		}
		return newPanel, nil
	}
	type outcome struct {
		result SubscriptionResult
		err    error
	}
	oldDone := make(chan outcome, 1)
	go func() {
		result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		oldDone <- outcome{result: result, err: err}
	}()
	<-oldStarted

	srv.APIToken = "new-token"
	srv.HostOverride = "new.example:8443"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	newResult, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if err != nil || !slices.Equal(newResult.Links, []string{"vless://new-connection"}) {
		t.Fatalf("new result=%+v err=%v", newResult, err)
	}
	close(releaseOld)
	oldResult := <-oldDone
	if !errors.Is(oldResult.err, ErrSubscriptionUnavailable) || len(oldResult.result.Links) != 0 {
		t.Fatalf("old result=%+v err=%v", oldResult.result, oldResult.err)
	}

	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for key, value := range a.links.values {
		if key.ServerID == srv.ID && (key.EffectiveHost == "old.example:443" || slices.Contains(value.Links, "vless://old-connection")) {
			t.Fatalf("old connection populated cache: key=%+v value=%+v", key, value)
		}
	}
}

func TestResolveSubscriptionDoesNotTrustCompletedInventoryFromOldConnection(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "old-token")
	oldPanel := activePanel("other", "vless://other")
	newPanel := activePanel("target", "vless://new-target")
	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(candidate storage.Server) (xui.PanelAPI, error) {
		if candidate.APIToken == "old-token" {
			return oldPanel, nil
		}
		return newPanel, nil
	}
	a.RefreshNow(context.Background())
	if _, ok := a.Snapshot().Servers[0].Groups["other"]; !ok {
		t.Fatalf("old inventory missing: %+v", a.Snapshot().Servers[0])
	}

	srv.APIToken = "new-token"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	result, err := a.ResolveSubscription(context.Background(), user.ID, "target")
	if err != nil || !slices.Equal(result.Links, []string{"vless://new-target"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRefreshFencesBlockedOldConnectionBeforeNewerPublication(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "old-token")

	oldStarted := make(chan struct{})
	oldCanceled := make(chan struct{})
	releaseOld := make(chan struct{})
	oldPanel := activePanel("old", "vless://old")
	oldClients := append([]xui.ClientSummary(nil), oldPanel.clients...)
	oldPanel.clientsFn = func(ctx context.Context) ([]xui.ClientSummary, error) {
		close(oldStarted)
		go func() {
			<-ctx.Done()
			close(oldCanceled)
		}()
		<-releaseOld
		return oldClients, nil
	}
	newFetched := make(chan struct{})
	newPanel := activePanel("new", "vless://new")
	newClients := append([]xui.ClientSummary(nil), newPanel.clients...)
	newPanel.clientsFn = func(context.Context) ([]xui.ClientSummary, error) {
		close(newFetched)
		return newClients, nil
	}

	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(candidate storage.Server) (xui.PanelAPI, error) {
		if candidate.APIToken == "old-token" {
			return oldPanel, nil
		}
		return newPanel, nil
	}
	oldDone := make(chan struct{})
	go func() {
		a.RefreshNow(context.Background())
		close(oldDone)
	}()
	<-oldStarted

	srv.APIToken = "new-token"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	newDone := make(chan struct{})
	go func() {
		a.RefreshNow(context.Background())
		close(newDone)
	}()

	select {
	case <-oldCanceled:
		close(releaseOld)
	case <-newFetched:
		// The unfenced implementation lets the newer refresh complete first;
		// release the old call so it deterministically overwrites that result.
		close(releaseOld)
	case <-time.After(time.Second):
		close(releaseOld)
		t.Fatal("neither old cancellation nor newer fetch was observed")
	}
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old refresh did not finish")
	}
	select {
	case <-newDone:
	case <-time.After(time.Second):
		t.Fatal("new refresh did not finish")
	}

	snapshot := a.Snapshot()
	if len(snapshot.Servers) != 1 {
		t.Fatalf("snapshot=%+v", snapshot.Servers)
	}
	if _, ok := snapshot.Servers[0].Groups["new"]; !ok {
		t.Fatalf("newer inventory was overwritten: %+v", snapshot.Servers[0])
	}
	if _, ok := snapshot.Servers[0].Groups["old"]; ok {
		t.Fatalf("old inventory reached final snapshot: %+v", snapshot.Servers[0])
	}
	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for _, value := range a.links.values {
		if slices.Contains(value.Links, "vless://old") {
			t.Fatalf("old refresh populated cache: %+v", value)
		}
	}
}

func TestDeletionCancelsHeldDiscoveryAndPreventsLateCachePopulation(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	panel := activePanel("group", "vless://deleted")
	clients := append([]xui.ClientSummary(nil), panel.clients...)
	panel.clientsFn = func(ctx context.Context) ([]xui.ClientSummary, error) {
		close(started)
		go func() {
			<-ctx.Done()
			close(canceled)
		}()
		<-release
		return clients, nil
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	type outcome struct {
		result SubscriptionResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		done <- outcome{result: result, err: err}
	}()
	<-started
	if err := store.DeleteServer(user.ID, srv.ID); err != nil {
		t.Fatal(err)
	}
	a.RefreshNow(context.Background())
	close(release)
	got := <-done
	if !errors.Is(got.err, ErrSubscriptionUnavailable) || len(got.result.Links) != 0 {
		t.Fatalf("deleted resolver result=%+v err=%v", got.result, got.err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("deleted server connection context was not canceled")
	}
	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for key := range a.links.values {
		if key.ServerID == srv.ID {
			t.Fatalf("deleted server repopulated cache: %+v", key)
		}
	}
}

func TestRefreshOmitsServerDeletedWhileOnlyRefreshIsInFlight(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	started := make(chan struct{})
	release := make(chan struct{})
	panel := activePanel("group", "vless://deleted")
	clients := append([]xui.ClientSummary(nil), panel.clients...)
	panel.clientsFn = func(context.Context) ([]xui.ClientSummary, error) {
		close(started)
		<-release
		return clients, nil
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	done := make(chan struct{})
	go func() {
		a.RefreshNow(context.Background())
		close(done)
	}()
	<-started
	if err := store.DeleteServer(user.ID, srv.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh did not finish")
	}

	if got := a.Snapshot().Servers; len(got) != 0 {
		t.Fatalf("deleted server published by in-flight refresh: %+v", got)
	}
	a.mu.Lock()
	current := a.clients[srv.ID]
	a.mu.Unlock()
	if current != nil {
		t.Fatalf("deleted client resurrected: %+v", current.srv)
	}
	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for key := range a.links.values {
		if key.ServerID == srv.ID {
			t.Fatalf("deleted server link cache resurrected: %+v", key)
		}
	}
}

func TestResolveSubscriptionCoalescesConcurrentColdDiscovery(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	panel := activePanel("group", "vless://coalesced")
	clients := append([]xui.ClientSummary(nil), panel.clients...)
	panel.clientsFn = func(context.Context) ([]xui.ClientSummary, error) {
		started <- struct{}{}
		<-release
		return clients, nil
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	type outcome struct {
		result SubscriptionResult
		err    error
	}
	results := make(chan outcome, 2)
	go func() {
		result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		results <- outcome{result: result, err: err}
	}()
	<-started
	go func() {
		result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		results <- outcome{result: result, err: err}
	}()
	waitForDiscoveryWaiters(t, a, srv.ID, "group", 1)
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || !slices.Equal(got.result.Links, []string{"vless://coalesced"}) {
			t.Fatalf("outcome=%+v", got)
		}
	}
	panel.mu.Lock()
	clientCalls, linkCalls := panel.clientCalls, panel.linkCalls
	panel.mu.Unlock()
	if clientCalls != 1 || linkCalls != 1 {
		t.Fatalf("clientCalls=%d linkCalls=%d", clientCalls, linkCalls)
	}
}

func TestResolveSubscriptionDiscoveryPanicDoesNotStrandWaiters(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	panel := activePanel("group", "vless://unused")
	panel.clientsFn = func(context.Context) ([]xui.ClientSummary, error) {
		started <- struct{}{}
		<-release
		panic("discovery payload secret")
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	type outcome struct {
		err       error
		recovered any
	}
	results := make(chan outcome, 2)
	call := func() {
		var got outcome
		func() {
			defer func() { got.recovered = recover() }()
			_, got.err = a.ResolveSubscription(context.Background(), user.ID, "group")
		}()
		results <- got
	}
	go call()
	<-started
	go call()
	waitForDiscoveryWaiters(t, a, srv.ID, "group", 1)
	close(release)
	for range 2 {
		got := <-results
		if got.recovered != nil || !errors.Is(got.err, ErrSubscriptionUnavailable) {
			t.Fatalf("outcome err=%v recovered=%v", got.err, got.recovered)
		}
	}
	panel.mu.Lock()
	clientCalls := panel.clientCalls
	panel.clientsFn = nil
	panel.mu.Unlock()
	if clientCalls != 1 {
		t.Fatalf("panic discovery calls=%d", clientCalls)
	}
	a.mu.Lock()
	remainingFlights := len(a.discoveries)
	a.mu.Unlock()
	if remainingFlights != 0 {
		t.Fatalf("discovery flights retained after panic=%d", remainingFlights)
	}
	retry, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if err != nil || !slices.Equal(retry.Links, []string{"vless://unused"}) {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}

func waitForDiscoveryWaiters(t *testing.T, a *Aggregator, serverID int64, subID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		joined := 0
		for key, flight := range a.discoveries {
			if key.ServerID == serverID && key.SubID == subID {
				joined = flight.waiters
				break
			}
		}
		a.mu.Unlock()
		if joined >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("discovery waiter did not join server=%d subID=%q", serverID, subID)
}

func TestResolveSubscriptionFetchesExactCompletedGroupsAndDoesNotRediscoverAbsence(t *testing.T) {
	store, user, _ := testStore(t)
	present := createServer(t, store, user.ID, "present", "token-a")
	absent := createServer(t, store, user.ID, "absent", "token-b")
	presentPanel := activePanel("exact", "trojan://from-panel")
	absentPanel := activePanel("other", "vless://other")
	a := newTestAggregator(store, map[int64]*fakePanel{present.ID: presentPanel, absent.ID: absentPanel}, nil)
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{
		completedSnapshot(present, "exact"), completedSnapshot(absent, "other"),
	}, BuiltAt: time.Now()})

	result, err := a.ResolveSubscription(context.Background(), user.ID, "exact")
	if err != nil || !slices.Equal(result.Links, []string{"trojan://from-panel"}) || result.Partial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	presentPanel.mu.Lock()
	presentClients, presentLinks := presentPanel.clientCalls, presentPanel.linkCalls
	presentPanel.mu.Unlock()
	absentPanel.mu.Lock()
	absentClients, absentLinks := absentPanel.clientCalls, absentPanel.linkCalls
	absentPanel.mu.Unlock()
	if presentClients != 0 || presentLinks != 1 || absentClients != 0 || absentLinks != 0 {
		t.Fatalf("present clients/links=%d/%d absent=%d/%d", presentClients, presentLinks, absentClients, absentLinks)
	}

	_, err = a.ResolveSubscription(context.Background(), user.ID, "prefix-exact")
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("inexact err=%v", err)
	}
	if presentPanel.clientCalls != 0 || absentPanel.clientCalls != 0 {
		t.Fatal("completed inventories were rediscovered for an absent group")
	}
}

func TestResolveSubscriptionDiscoversOwnedUninitializedPanels(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "cold", "token")
	panel := activePanel("cold-group", "ss://cold-link")
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)

	result, err := a.ResolveSubscription(context.Background(), user.ID, "cold-group")
	if err != nil || !slices.Equal(result.Links, []string{"ss://cold-link"}) || result.Partial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if panel.clientCalls != 1 || panel.linkCalls != 1 {
		t.Fatalf("clients=%d links=%d", panel.clientCalls, panel.linkCalls)
	}
}

func TestResolveSubscriptionReturnsPartialSortedDeduplicatedLinks(t *testing.T) {
	store, user, _ := testStore(t)
	first := createServer(t, store, user.ID, "first", "token-a")
	second := createServer(t, store, user.ID, "second", "token-b")
	third := createServer(t, store, user.ID, "third", "token-c")
	firstPanel := activePanel("group", "vmess://b")
	firstPanel.links["group"] = []string{"vmess://b", "vless://a", "vless://a"}
	secondPanel := activePanel("group", "vless://a")
	thirdPanel := activePanel("group", "unused")
	thirdPanel.linkErrs = map[string]error{"group": errors.New("panel failure with secret")}
	a := newTestAggregator(store, map[int64]*fakePanel{first.ID: firstPanel, second.ID: secondPanel, third.ID: thirdPanel}, nil)
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{
		completedSnapshot(first, "group"), completedSnapshot(second, "group"), completedSnapshot(third, "group"),
	}, BuiltAt: time.Now()})

	result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if err != nil || !result.Partial || !slices.Equal(result.Links, []string{"vless://a", "vmess://b"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestResolveSubscriptionClassifiesEmptyAbsenceAndFailures(t *testing.T) {
	t.Run("successful empty is not found", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "token")
		panel := activePanel("group", "")
		panel.links["group"] = nil
		a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
		a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "group")}})
		_, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		if !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("relevant failure is unavailable", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "token")
		panel := activePanel("group", "")
		panel.linkErrs = map[string]error{"group": errors.New("raw panel secret")}
		a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
		a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "group")}})
		_, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		if !errors.Is(err, ErrSubscriptionUnavailable) || strings.Contains(err.Error(), "raw panel secret") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cold discovery failure is unavailable", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "token")
		panel := activePanel("group", "")
		panel.clientsErr = errors.New("discovery secret")
		a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
		_, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		if !errors.Is(err, ErrSubscriptionUnavailable) || strings.Contains(err.Error(), "discovery secret") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestResolveSubscriptionCoalescesConcurrentColdLinkFetches(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("group", "")
	started := make(chan struct{})
	release := make(chan struct{})
	panel.linksFn = func(ctx context.Context, _, _ string) ([]string, error) {
		close(started)
		select {
		case <-release:
			return []string{"vless://coalesced"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "group")}})

	type outcome struct {
		result SubscriptionResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
			results <- outcome{result: result, err: err}
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("link fetch did not start")
	}
	time.Sleep(20 * time.Millisecond)
	panel.mu.Lock()
	calls := panel.linkCalls
	panel.mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent link calls=%d", calls)
	}
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || !slices.Equal(got.result.Links, []string{"vless://coalesced"}) {
			t.Fatalf("outcome=%+v", got)
		}
	}
}

func TestResolveSubscriptionBoundsColdDiscoveryWithContext(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("group", "")
	panel.clientsFn = func(ctx context.Context) ([]xui.ClientSummary, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	a.cfg.RequestTimeout = 20 * time.Millisecond

	started := time.Now()
	_, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if !errors.Is(err, ErrSubscriptionUnavailable) || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestResolveSubscriptionPurgesCachedLinksBeforeServingChangedConnection(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "old-token")
	panel := activePanel("group", "vless://new-connection")
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	if _, err := a.clientFor(srv); err != nil {
		t.Fatal(err)
	}
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "group")}})
	mustRefreshLinkCache(t, a.links, linkKey{ServerID: srv.ID, SubID: "group", EffectiveHost: publicHost(srv)}, []string{"vless://old-connection"})

	srv.APIToken = "new-token"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if err != nil || !slices.Equal(result.Links, []string{"vless://new-connection"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if panel.linkCalls != 1 {
		t.Fatalf("new connection link calls=%d", panel.linkCalls)
	}
}

func TestResolveSubscriptionBoundsWaitForLinkCacheSlot(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	panel := activePanel("group", "vless://eventual")
	a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: panel}, nil)
	a.cfg.RequestTimeout = 20 * time.Millisecond
	a.links = newLinkCache(1)
	a.fetcher.links = a.links
	a.snap.Store(&Snapshot{Servers: []ServerSnapshot{completedSnapshot(srv, "group")}})

	slotStarted := make(chan struct{})
	releaseSlot := make(chan struct{})
	slotDone := make(chan struct{})
	go func() {
		defer close(slotDone)
		_, _ = a.links.Refresh(context.Background(), linkKey{ServerID: 99, SubID: "block", EffectiveHost: "host"}, func(context.Context) ([]string, error) {
			close(slotStarted)
			<-releaseSlot
			return nil, nil
		})
	}()
	<-slotStarted

	result := make(chan error, 1)
	go func() {
		_, err := a.ResolveSubscription(context.Background(), user.ID, "group")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrSubscriptionUnavailable) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(releaseSlot)
		<-slotDone
		<-result
		t.Fatal("resolver wait for a cache slot exceeded its request timeout")
	}
	close(releaseSlot)
	<-slotDone
}

func TestResolveSubscriptionRejectsPreloadedRowAfterNewConnectionInstalled(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "old-token")
	srv.HostOverride = "old.example:443"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	oldRows, err := store.ListServersByUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv.APIToken = "new-token"
	srv.HostOverride = "new.example:8443"
	if err := store.UpdateServer(&srv); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.ServerByID(user.ID, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	newPanel := activePanel("group", "vless://new")
	var oldFactoryCalls, newFactoryCalls int
	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(candidate storage.Server) (xui.PanelAPI, error) {
		if candidate.APIToken == "old-token" {
			oldFactoryCalls++
			return activePanel("group", "vless://old"), nil
		}
		newFactoryCalls++
		return newPanel, nil
	}
	newClient, err := a.clientFor(*fresh)
	if err != nil {
		t.Fatal(err)
	}
	newEpoch := newClient.epoch

	_, err = a.resolveSubscriptionServers(context.Background(), user.ID, "group", oldRows)
	if !errors.Is(err, ErrSubscriptionUnavailable) {
		t.Fatalf("stale resolver err=%v", err)
	}
	a.mu.Lock()
	current := a.clients[srv.ID]
	currentEpoch := a.epochs[srv.ID]
	a.mu.Unlock()
	if current != newClient || currentEpoch != newEpoch || newClient.ctx.Err() != nil {
		t.Fatalf("new client was replaced/canceled: current=%p new=%p epoch=%d want=%d err=%v", current, newClient, currentEpoch, newEpoch, newClient.ctx.Err())
	}
	if oldFactoryCalls != 0 || newFactoryCalls != 1 {
		t.Fatalf("factory old/new=%d/%d", oldFactoryCalls, newFactoryCalls)
	}

	result, err := a.ResolveSubscription(context.Background(), user.ID, "group")
	if err != nil || !slices.Equal(result.Links, []string{"vless://new"}) {
		t.Fatalf("new result=%+v err=%v", result, err)
	}
	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for key, value := range a.links.values {
		if key.ServerID == srv.ID && (key.Epoch != newEpoch || slices.Contains(value.Links, "vless://old")) {
			t.Fatalf("stale cache survived: key=%+v value=%+v", key, value)
		}
	}
}

func TestResolveSubscriptionRejectsPreloadedDeletedRowWithoutResurrection(t *testing.T) {
	store, user, _ := testStore(t)
	srv := createServer(t, store, user.ID, "node", "token")
	oldRows, err := store.ListServersByUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	panel := activePanel("group", "vless://deleted")
	factoryCalls := 0
	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(storage.Server) (xui.PanelAPI, error) {
		factoryCalls++
		return panel, nil
	}
	sc, err := a.clientFor(srv)
	if err != nil {
		t.Fatal(err)
	}
	mustRefreshLinkCache(t, a.links, linkKey{
		ServerID: srv.ID, Epoch: sc.epoch, SubID: "group", EffectiveHost: publicHost(srv),
	}, []string{"vless://deleted"})
	if err := store.DeleteServer(user.ID, srv.ID); err != nil {
		t.Fatal(err)
	}
	a.reconcileConnections(nil)
	epochAfterDeletion := a.serverEpoch(srv.ID)

	_, err = a.resolveSubscriptionServers(context.Background(), user.ID, "group", oldRows)
	if !errors.Is(err, ErrSubscriptionUnavailable) {
		t.Fatalf("stale deleted resolver err=%v", err)
	}
	if factoryCalls != 1 || a.serverEpoch(srv.ID) != epochAfterDeletion {
		t.Fatalf("deleted row resurrected/churned: factory=%d epoch=%d want=%d", factoryCalls, a.serverEpoch(srv.ID), epochAfterDeletion)
	}
	a.mu.Lock()
	current := a.clients[srv.ID]
	a.mu.Unlock()
	if current != nil {
		t.Fatalf("deleted client resurrected: %+v", current.srv)
	}
	a.links.mu.RLock()
	defer a.links.mu.RUnlock()
	for key := range a.links.values {
		if key.ServerID == srv.ID {
			t.Fatalf("deleted cache resurrected: %+v", key)
		}
	}
}

func TestResolveSubscriptionRejectsObservedStaleRowsWithoutCachedClient(t *testing.T) {
	t.Run("rotated", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "old-token")
		oldRows, err := store.ListServersByUser(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		srv.APIToken = "new-token"
		if err := store.UpdateServer(&srv); err != nil {
			t.Fatal(err)
		}
		freshRows, err := store.ListServersByUser(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		factoryCalls := 0
		a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
		a.panelFactory = func(storage.Server) (xui.PanelAPI, error) {
			factoryCalls++
			return activePanel("group", "vless://unexpected"), nil
		}
		a.reconcileConnections(freshRows)
		observedEpoch := a.serverEpoch(srv.ID)

		_, err = a.resolveSubscriptionServers(context.Background(), user.ID, "group", oldRows)
		if !errors.Is(err, ErrSubscriptionUnavailable) || factoryCalls != 0 || a.serverEpoch(srv.ID) != observedEpoch {
			t.Fatalf("err=%v factory=%d epoch=%d want=%d", err, factoryCalls, a.serverEpoch(srv.ID), observedEpoch)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "token")
		oldRows, err := store.ListServersByUser(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
		a.reconcileConnections(oldRows)
		if err := store.DeleteServer(user.ID, srv.ID); err != nil {
			t.Fatal(err)
		}
		a.reconcileConnections(nil)
		factoryCalls := 0
		a.panelFactory = func(storage.Server) (xui.PanelAPI, error) {
			factoryCalls++
			return activePanel("group", "vless://unexpected"), nil
		}
		observedEpoch := a.serverEpoch(srv.ID)

		_, err = a.resolveSubscriptionServers(context.Background(), user.ID, "group", oldRows)
		if !errors.Is(err, ErrSubscriptionUnavailable) || factoryCalls != 0 || a.serverEpoch(srv.ID) != observedEpoch {
			t.Fatalf("err=%v factory=%d epoch=%d want=%d", err, factoryCalls, a.serverEpoch(srv.ID), observedEpoch)
		}
	})
}

func TestReconcileConnectionsRejectsStaleRows(t *testing.T) {
	t.Run("rotation", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "old-token")
		oldRows, err := store.ListServersByUser(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		srv.APIToken = "new-token"
		if err := store.UpdateServer(&srv); err != nil {
			t.Fatal(err)
		}
		fresh, err := store.ServerByID(user.ID, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: activePanel("group", "vless://new")}, nil)
		newClient, err := a.clientFor(*fresh)
		if err != nil {
			t.Fatal(err)
		}
		epoch := newClient.epoch

		a.reconcileConnections(oldRows)
		a.mu.Lock()
		current := a.clients[srv.ID]
		a.mu.Unlock()
		if current != newClient || newClient.ctx.Err() != nil || a.serverEpoch(srv.ID) != epoch {
			t.Fatalf("stale reconcile replaced new client: current=%p new=%p err=%v epoch=%d want=%d", current, newClient, newClient.ctx.Err(), a.serverEpoch(srv.ID), epoch)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		store, user, _ := testStore(t)
		srv := createServer(t, store, user.ID, "node", "token")
		oldRows, err := store.ListServersByUser(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := newTestAggregator(store, map[int64]*fakePanel{srv.ID: activePanel("group", "vless://old")}, nil)
		sc, err := a.clientFor(srv)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteServer(user.ID, srv.ID); err != nil {
			t.Fatal(err)
		}

		a.reconcileConnections(oldRows)
		a.mu.Lock()
		current := a.clients[srv.ID]
		a.mu.Unlock()
		if current != nil || sc.ctx.Err() == nil {
			t.Fatalf("stale reconcile retained deleted client: current=%p canceled=%v", current, sc.ctx.Err())
		}
	})
}
