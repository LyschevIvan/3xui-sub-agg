package aggregator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

func TestNativeFetcherJoinsClientMembershipsToSlimInbounds(t *testing.T) {
	recordID := 7
	api := &fakeNativeAPI{
		status: xui.ServerStatus{PanelVersion: "3.4.2"},
		clients: []xui.ClientSummary{
			{RecordID: &recordID, Email: "z@example", SubID: "same", Enable: true, InboundIDs: []int{2, 1, 1}},
			{Email: "a@example", SubID: "same", Enable: false, InboundIDs: []int{1}},
			{Email: "orphan@example", SubID: "same", Enable: true, InboundIDs: []int{99}},
			{Email: "ss@example", SubID: "other", Enable: true, InboundIDs: []int{3}},
			{Email: "no-sub@example", Enable: true, InboundIDs: []int{1}},
		},
		inbounds: []xui.InboundSummary{
			{ID: 1, Remark: "primary", Enable: true, Port: 443, Protocol: "vless", StreamSettings: []byte(`{"network":"ws","security":"tls"}`)},
			{ID: 2, Remark: "disabled", Enable: false, Port: 8443, Protocol: "trojan", StreamSettings: []byte(`{"network":"grpc","security":"reality"}`)},
			{ID: 3, Remark: "legacy", Enable: true, Port: 8388, Protocol: "shadowsocks"},
		},
		subLinks: map[string][]string{
			"same":  {"vless://same"},
			"other": {"ss://other"},
		},
	}
	fetcher := nativeFetcher{links: newLinkCache(4), workers: 2}
	srv := storage.Server{ID: 11, UserID: 22, Name: "node"}

	snapshot, err := fetcher.Fetch(context.Background(), srv, api, "public.example:443")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.ID != 11 || snapshot.UserID != 22 || snapshot.Name != "node" || snapshot.PublicHost != "public.example:443" {
		t.Fatalf("metadata=%+v", snapshot)
	}
	if snapshot.PanelVersion != "3.4.2" || snapshot.State != ServerOK || snapshot.AttemptedAt.IsZero() || snapshot.FetchedAt.Before(snapshot.AttemptedAt) {
		t.Fatalf("status/times=%+v", snapshot)
	}
	if len(snapshot.Inbounds) != 3 {
		t.Fatalf("inbounds=%+v", snapshot.Inbounds)
	}
	if got := snapshot.Inbounds[0]; got.Protocol != "vless" || got.Network != "ws" || got.Security != "tls" || !got.Enable {
		t.Fatalf("vless inbound=%+v", got)
	}
	if got := snapshot.Inbounds[1]; got.Protocol != "trojan" || got.Network != "grpc" || got.Security != "reality" || got.Enable {
		t.Fatalf("trojan inbound=%+v", got)
	}
	if got := snapshot.Inbounds[2]; got.Protocol != "shadowsocks" || got.Network != "tcp" || got.Security != "none" {
		t.Fatalf("shadowsocks inbound=%+v", got)
	}

	group := snapshot.Groups["same"]
	if len(snapshot.Groups) != 2 || len(group.Records) != 3 {
		t.Fatalf("groups=%+v", snapshot.Groups)
	}
	if group.Records[0].Email != "a@example" || group.Records[0].Enabled ||
		group.Records[1].Email != "orphan@example" || !group.Records[1].Enabled ||
		group.Records[2].Email != "z@example" || !group.Records[2].Enabled {
		t.Fatalf("records=%+v", group.Records)
	}
	if !slices.Equal(group.Records[2].InboundIDs, []int{1, 2}) {
		t.Fatalf("normalized ids=%v", group.Records[2].InboundIDs)
	}
	if _, ok := snapshot.Groups[""]; ok {
		t.Fatalf("empty group retained: %+v", snapshot.Groups[""])
	}

	recordID = 99
	api.clients[0].InboundIDs[0] = 99
	if group.Records[2].RecordID == nil || *group.Records[2].RecordID != 7 || !slices.Equal(group.Records[2].InboundIDs, []int{1, 2}) {
		t.Fatalf("snapshot aliases inventory: %+v", group.Records[2])
	}
}

func TestNativeFetcherFetchesOneSubLinksPerActiveSubID(t *testing.T) {
	api := &fakeNativeAPI{
		status: xui.ServerStatus{PanelVersion: "3.4.2"},
		clients: []xui.ClientSummary{
			{Email: "one", SubID: "active", Enable: true, InboundIDs: []int{1}},
			{Email: "two", SubID: "active", Enable: true, InboundIDs: []int{1}},
			{Email: "disabled", SubID: "disabled", Enable: false, InboundIDs: []int{1}},
			{Email: "disabled-inbound", SubID: "disabled-inbound", Enable: true, InboundIDs: []int{2}},
			{Email: "orphan", SubID: "orphan", Enable: true, InboundIDs: []int{99}},
		},
		inbounds: []xui.InboundSummary{
			{ID: 1, Enable: true, Protocol: "vless"},
			{ID: 2, Enable: false, Protocol: "vless"},
		},
		subLinks: map[string][]string{"active": {"vless://active"}},
	}
	cache := newLinkCache(4)
	fetcher := nativeFetcher{links: cache, workers: 3}

	snapshot, err := fetcher.Fetch(context.Background(), storage.Server{ID: 1}, api, "host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snapshot.Groups) != 4 {
		t.Fatalf("groups=%+v", snapshot.Groups)
	}
	if got := api.subLinkCallCount("active"); got != 1 {
		t.Fatalf("active calls=%d", got)
	}
	for _, subID := range []string{"disabled", "disabled-inbound", "orphan"} {
		if got := api.subLinkCallCount(subID); got != 0 {
			t.Fatalf("%s calls=%d", subID, got)
		}
		if _, ok := cache.Get(linkKey{ServerID: 1, SubID: subID, EffectiveHost: "host"}); ok {
			t.Fatalf("inactive key cached: %s", subID)
		}
	}
	if calls := api.subLinkCalls(); len(calls) != 1 || calls[0].host != "host" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestNativeFetcherKeepsOtherLinksWhenOneSubIDFails(t *testing.T) {
	failed := errors.New("subscription refresh failed")
	api := activeGroupsAPI("good", "bad")
	api.subLinks = map[string][]string{"good": {"vless://b", "vless://a", "vless://a"}}
	api.subErrors = map[string]error{"bad": failed}
	cache := newLinkCache(4)
	fetcher := nativeFetcher{links: cache, workers: 2}

	snapshot, err := fetcher.Fetch(context.Background(), storage.Server{ID: 1}, api, "host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.State != ServerDegraded || !errors.Is(snapshot.SyncErr, failed) {
		t.Fatalf("state=%q syncErr=%v", snapshot.State, snapshot.SyncErr)
	}
	if len(snapshot.Groups) != 2 {
		t.Fatalf("groups=%+v", snapshot.Groups)
	}
	good, ok := cache.Get(linkKey{ServerID: 1, SubID: "good", EffectiveHost: "host"})
	if !ok || strings.Join(good.Links, ",") != "vless://a,vless://b" {
		t.Fatalf("good value=%+v ok=%v", good, ok)
	}
	if _, ok := cache.Get(linkKey{ServerID: 1, SubID: "bad", EffectiveHost: "host"}); ok {
		t.Fatal("failed uncached group unexpectedly gained a value")
	}
}

func TestNativeFetcherOrdersSyncErrorsBySortedKey(t *testing.T) {
	const (
		firstCapability   = "a-raw-subid-secret"
		secondCapability  = "b-raw-subid-secret"
		successCapability = "c-raw-subid-secret"
		sensitiveLink     = "vless://sensitive-link"
	)
	firstFailure := errors.New("subscription links: first safe failure")
	secondFailure := errors.New("subscription links: second safe failure")
	want := firstFailure.Error() + "\n" + secondFailure.Error()

	var firstResult string
	for attempt := range 10 {
		t.Run(fmt.Sprintf("attempt-%d", attempt), func(t *testing.T) {
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			successStarted := make(chan struct{})
			api := activeGroupsAPI(successCapability, secondCapability, firstCapability)
			api.subLinksFn = func(_ context.Context, subID, _ string) ([]string, error) {
				switch subID {
				case firstCapability:
					close(firstStarted)
					<-releaseFirst
					return nil, firstFailure
				case secondCapability:
					<-firstStarted
					return nil, secondFailure
				case successCapability:
					close(successStarted)
					return []string{sensitiveLink}, nil
				default:
					return nil, errors.New("unexpected test subscription key")
				}
			}

			type result struct {
				snapshot ServerSnapshot
				err      error
			}
			done := make(chan result, 1)
			go func() {
				snapshot, err := (&nativeFetcher{links: newLinkCache(4), workers: 2}).Fetch(
					context.Background(), storage.Server{ID: 1}, api, "host",
				)
				done <- result{snapshot: snapshot, err: err}
			}()

			select {
			case <-successStarted:
				// The worker can only begin this third sorted job after recording
				// the second job's earlier-completing failure.
			case <-time.After(time.Second):
				t.Fatal("reverse-completion barrier was not reached")
			}
			close(releaseFirst)

			var got result
			select {
			case got = <-done:
			case <-time.After(time.Second):
				t.Fatal("Fetch did not finish")
			}
			if got.err != nil {
				t.Fatalf("Fetch: %v", got.err)
			}
			if got.snapshot.State != ServerDegraded || !errors.Is(got.snapshot.SyncErr, firstFailure) || !errors.Is(got.snapshot.SyncErr, secondFailure) {
				t.Fatalf("state=%q syncErr=%v", got.snapshot.State, got.snapshot.SyncErr)
			}
			if text := got.snapshot.SyncErr.Error(); text != want {
				t.Fatalf("SyncErr order=%q; want=%q", text, want)
			} else if strings.Contains(text, firstCapability) || strings.Contains(text, secondCapability) || strings.Contains(text, successCapability) || strings.Contains(text, sensitiveLink) {
				t.Fatalf("SyncErr exposed sensitive data: %q", text)
			} else if attempt == 0 {
				firstResult = text
			} else if text != firstResult {
				t.Fatalf("attempt %d SyncErr=%q; first=%q", attempt, text, firstResult)
			}
		})
	}
}

func TestNativeFetcherUsesStaleLinkOnRefreshError(t *testing.T) {
	failed := errors.New("subscription refresh failed")
	cache := newLinkCache(2)
	key := linkKey{ServerID: 1, SubID: "stale", EffectiveHost: "host"}
	before := mustRefreshLinkCache(t, cache, key, []string{"vless://stale"})
	api := activeGroupsAPI("stale")
	api.subErrors = map[string]error{"stale": failed}

	snapshot, err := (&nativeFetcher{links: cache, workers: 1}).Fetch(context.Background(), storage.Server{ID: 1}, api, "host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.State != ServerDegraded || !errors.Is(snapshot.SyncErr, failed) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	after, ok := cache.Get(key)
	if !ok || strings.Join(after.Links, ",") != "vless://stale" || !after.FetchedAt.Equal(before.FetchedAt) {
		t.Fatalf("stale value changed: before=%+v after=%+v ok=%v", before, after, ok)
	}
}

func TestNativeFetcherDoesNotPruneAfterInventoryFailure(t *testing.T) {
	discoveryFailed := errors.New("inventory failed")
	for _, tc := range []struct {
		name string
		api  *fakeNativeAPI
	}{
		{"validate", &fakeNativeAPI{validateErr: discoveryFailed}},
		{"clients", &fakeNativeAPI{status: xui.ServerStatus{PanelVersion: "3.4.2"}, clientsErr: discoveryFailed}},
		{"inbounds", &fakeNativeAPI{status: xui.ServerStatus{PanelVersion: "3.4.2"}, inboundsErr: discoveryFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newLinkCache(2)
			key := linkKey{ServerID: 1, SubID: "previous", EffectiveHost: "old-host"}
			before := mustRefreshLinkCache(t, cache, key, []string{"vless://previous"})

			_, err := (&nativeFetcher{links: cache, workers: 1}).Fetch(context.Background(), storage.Server{ID: 1}, tc.api, "new-host")
			if !errors.Is(err, discoveryFailed) {
				t.Fatalf("err=%v", err)
			}
			after, ok := cache.Get(key)
			if !ok || strings.Join(after.Links, ",") != "vless://previous" || !after.FetchedAt.Equal(before.FetchedAt) {
				t.Fatalf("cache pruned after discovery error: value=%+v ok=%v", after, ok)
			}
		})
	}
}

func TestNativeFetcherPrunesRemovedGroupsAfterCompleteInventory(t *testing.T) {
	cache := newLinkCache(4)
	active := linkKey{ServerID: 1, SubID: "active", EffectiveHost: "new-host"}
	removed := linkKey{ServerID: 1, SubID: "removed", EffectiveHost: "new-host"}
	oldHost := linkKey{ServerID: 1, SubID: "active", EffectiveHost: "old-host"}
	inactive := linkKey{ServerID: 1, SubID: "inactive", EffectiveHost: "new-host"}
	serverTwo := linkKey{ServerID: 2, SubID: "other", EffectiveHost: "new-host"}
	for _, key := range []linkKey{active, removed, oldHost, inactive, serverTwo} {
		mustRefreshLinkCache(t, cache, key, []string{"vless://old"})
	}

	api := &fakeNativeAPI{
		status: xui.ServerStatus{PanelVersion: "3.4.2"},
		clients: []xui.ClientSummary{
			{Email: "active", SubID: "active", Enable: true, InboundIDs: []int{1}},
			{Email: "inactive", SubID: "inactive", Enable: true, InboundIDs: []int{2}},
		},
		inbounds: []xui.InboundSummary{
			{ID: 1, Enable: true, Protocol: "vless"},
			{ID: 2, Enable: false, Protocol: "vless"},
		},
		subLinks: map[string][]string{"active": {"vless://new"}},
	}
	snapshot, err := (&nativeFetcher{links: cache, workers: 2}).Fetch(context.Background(), storage.Server{ID: 1}, api, "new-host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snapshot.Groups["inactive"].Records) != 1 {
		t.Fatalf("inactive group missing from snapshot: %+v", snapshot.Groups)
	}
	if value, ok := cache.Get(active); !ok || strings.Join(value.Links, ",") != "vless://new" {
		t.Fatalf("active value=%+v ok=%v", value, ok)
	}
	for _, key := range []linkKey{removed, oldHost, inactive} {
		if _, ok := cache.Get(key); ok {
			t.Fatalf("obsolete key retained: %+v", key)
		}
	}
	if _, ok := cache.Get(serverTwo); !ok {
		t.Fatal("other server key was pruned")
	}
	if api.subLinkCallCount("active") != 1 || api.subLinkCallCount("inactive") != 0 {
		t.Fatalf("calls=%+v", api.subLinkCalls())
	}
}

func TestNativeFetcherBoundsSubLinkWorkers(t *testing.T) {
	api := activeGroupsAPI("a", "b", "c", "d", "e", "f")
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	var running atomic.Int32
	var maxRunning atomic.Int32
	api.subLinksFn = func(context.Context, string, string) ([]string, error) {
		current := running.Add(1)
		for {
			maximum := maxRunning.Load()
			if current <= maximum || maxRunning.CompareAndSwap(maximum, current) {
				break
			}
		}
		started <- struct{}{}
		defer running.Add(-1)
		<-release
		return []string{"vless://ok"}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := (&nativeFetcher{links: newLinkCache(10), workers: 2}).Fetch(context.Background(), storage.Server{ID: 1}, api, "host")
		done <- err
	}()
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("more than two SubLinks calls started while both workers were blocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fetch did not finish")
	}
	if got := maxRunning.Load(); got > 2 {
		t.Fatalf("maxRunning=%d", got)
	}
}

type nativeSubLinkCall struct {
	subID string
	host  string
}

type fakeNativeAPI struct {
	mu sync.Mutex

	status      xui.ServerStatus
	validateErr error
	clients     []xui.ClientSummary
	clientsErr  error
	inbounds    []xui.InboundSummary
	inboundsErr error
	subLinks    map[string][]string
	subErrors   map[string]error
	subLinksFn  func(context.Context, string, string) ([]string, error)
	calls       []nativeSubLinkCall
}

func (f *fakeNativeAPI) Validate(context.Context) (xui.ServerStatus, error) {
	return f.status, f.validateErr
}

func (f *fakeNativeAPI) ListClients(context.Context) ([]xui.ClientSummary, error) {
	return f.clients, f.clientsErr
}

func (f *fakeNativeAPI) ListSlimInbounds(context.Context) ([]xui.InboundSummary, error) {
	return f.inbounds, f.inboundsErr
}

func (f *fakeNativeAPI) SubLinks(ctx context.Context, subID, host string) ([]string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, nativeSubLinkCall{subID: subID, host: host})
	fn := f.subLinksFn
	links := append([]string(nil), f.subLinks[subID]...)
	err := f.subErrors[subID]
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, subID, host)
	}
	return links, err
}

func (f *fakeNativeAPI) subLinkCallCount(subID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.subID == subID {
			count++
		}
	}
	return count
}

func (f *fakeNativeAPI) subLinkCalls() []nativeSubLinkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]nativeSubLinkCall(nil), f.calls...)
}

func activeGroupsAPI(subIDs ...string) *fakeNativeAPI {
	clients := make([]xui.ClientSummary, 0, len(subIDs))
	for _, subID := range subIDs {
		clients = append(clients, xui.ClientSummary{
			Email:      fmt.Sprintf("%s@example", subID),
			SubID:      subID,
			Enable:     true,
			InboundIDs: []int{1},
		})
	}
	return &fakeNativeAPI{
		status:   xui.ServerStatus{PanelVersion: "3.4.2"},
		clients:  clients,
		inbounds: []xui.InboundSummary{{ID: 1, Enable: true, Protocol: "vless"}},
	}
}
