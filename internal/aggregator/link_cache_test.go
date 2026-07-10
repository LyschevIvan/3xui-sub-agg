package aggregator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinkCacheCoalescesAndPreservesStale(t *testing.T) {
	cache := newLinkCache(2)
	key := linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "public.example:443"}

	var calls atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	fetch := func(context.Context) ([]string, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return []string{"vless://b", "vless://a", "vless://a", "", " \t "}, nil
	}

	type result struct {
		value linkValue
		err   error
	}
	results := make(chan result, 8)
	var entered sync.WaitGroup
	entered.Add(8)
	cache.mu.Lock()
	for range 8 {
		go func() {
			entered.Done()
			value, err := cache.GetOrFetch(context.Background(), key, fetch)
			results <- result{value: value, err: err}
		}()
	}
	entered.Wait()
	cache.mu.Unlock()

	<-started
	secondStarted := false
	select {
	case <-started:
		secondStarted = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	all := make([]linkValue, 0, 8)
	for range 8 {
		got := <-results
		if got.err != nil || strings.Join(got.value.Links, ",") != "vless://a,vless://b" {
			t.Fatalf("value=%+v err=%v", got.value, got.err)
		}
		all = append(all, got.value)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if secondStarted {
		t.Fatal("a second same-key callback started while the leader was blocked")
	}

	// Results from the flight and Get must not share mutable slice storage.
	all[0].Links[0] = "mutated"
	if all[1].Links[0] != "vless://a" {
		t.Fatalf("flight results share links: %v", all[1].Links)
	}
	before, ok := cache.Get(key)
	if !ok {
		t.Fatal("cached value missing")
	}
	before.Links[0] = "mutated again"
	unchanged, ok := cache.Get(key)
	if !ok || strings.Join(unchanged.Links, ",") != "vless://a,vless://b" {
		t.Fatalf("Get exposed cache storage: value=%+v ok=%v", unchanged, ok)
	}

	offline := errors.New("offline")
	value, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
		return nil, offline
	})
	if !errors.Is(err, offline) || strings.Join(value.Links, ",") != "vless://a,vless://b" {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	if !value.FetchedAt.Equal(unchanged.FetchedAt) {
		t.Fatalf("failed refresh changed timestamp: before=%v after=%v", unchanged.FetchedAt, value.FetchedAt)
	}
	if stored, ok := cache.Get(key); !ok || !stored.FetchedAt.Equal(unchanged.FetchedAt) {
		t.Fatalf("failed refresh overwrote cache: value=%+v ok=%v", stored, ok)
	}
}

func TestLinkCacheSuccessfulEmptyRefreshReplacesStale(t *testing.T) {
	cache := newLinkCache(1)
	key := linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "host"}
	mustRefreshLinkCache(t, cache, key, []string{"vless://stale"})

	value, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
		return []string{"", "  ", "\n"}, nil
	})
	if err != nil || len(value.Links) != 0 {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	stored, ok := cache.Get(key)
	if !ok || len(stored.Links) != 0 {
		t.Fatalf("stored=%+v ok=%v", stored, ok)
	}
}

func TestLinkCacheBoundsDistinctFetchesGlobally(t *testing.T) {
	cache := newLinkCache(2)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	var running atomic.Int32
	var maxRunning atomic.Int32
	var attempts atomic.Int32

	fetch := func(context.Context) ([]string, error) {
		attempts.Add(1)
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

	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(10)
	startLine := make(chan struct{})
	errs := make(chan error, 10)
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-startLine
			_, err := cache.Refresh(context.Background(), linkKey{
				ServerID: 1, SubID: string(rune('a' + i)), EffectiveHost: "host",
			}, fetch)
			errs <- err
		}(i)
	}

	ready.Wait()
	close(startLine)
	<-started
	<-started
	thirdStarted := false
	select {
	case <-started:
		thirdStarted = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if got := maxRunning.Load(); got > 2 {
		t.Fatalf("maxRunning=%d", got)
	}
	if thirdStarted {
		t.Fatal("a third distinct callback started while both global slots were blocked")
	}
	if got := attempts.Load(); got != 10 {
		t.Fatalf("attempts=%d", got)
	}
}

func TestLinkCacheKeysEffectiveHostExactly(t *testing.T) {
	cache := newLinkCache(2)
	var calls atomic.Int32
	fetch := func(context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"vless://ok"}, nil
	}

	for _, host := range []string{"public.example:443", "Public.example:443"} {
		if _, err := cache.GetOrFetch(context.Background(), linkKey{ServerID: 1, SubID: "sub", EffectiveHost: host}, fetch); err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestLinkCacheWaiterCanCancelWithoutCancelingLeader(t *testing.T) {
	cache := newLinkCache(1)
	key := linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "host"}
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
			close(started)
			<-release
			return []string{"vless://ok"}, nil
		})
		leaderDone <- err
	}()
	<-started

	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := cache.Refresh(waiterCtx, key, func(context.Context) ([]string, error) {
			t.Error("waiter unexpectedly started another fetch")
			return nil, nil
		})
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err=%v", err)
	}
}

func TestLinkCachePanicWakesWaitersAndAllowsRetry(t *testing.T) {
	cache := newLinkCache(1)
	key := linkKey{ServerID: 1, SubID: "secret-subid", EffectiveHost: "secret-host"}
	const panicValue = "vless://panic-secret-link"
	started := make(chan struct{})
	triggerPanic := make(chan struct{})
	leaderRecovered := make(chan any, 1)
	go func() {
		defer func() { leaderRecovered <- recover() }()
		_, _ = cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
			close(started)
			<-triggerPanic
			panic(panicValue)
		})
	}()
	<-started

	cache.mu.RLock()
	flight := cache.flights[key]
	cache.mu.RUnlock()
	if flight == nil {
		t.Fatal("leader flight was not registered")
	}
	type waiterResult struct {
		value linkValue
		err   error
	}
	waiterReady := make(chan struct{})
	waiterDone := make(chan waiterResult, 1)
	go func() {
		close(waiterReady)
		value, err := waitLinkFlight(context.Background(), flight)
		waiterDone <- waiterResult{value: value, err: err}
	}()
	<-waiterReady
	close(triggerPanic)

	select {
	case recovered := <-leaderRecovered:
		if recovered != panicValue {
			t.Fatalf("leader recovered=%v; want original panic", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("leader panic was not propagated")
	}

	var waiter waiterResult
	select {
	case waiter = <-waiterDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waiter was not awakened after callback panic")
	}
	if waiter.err == nil || waiter.err.Error() != "subscription links: fetch failed" {
		t.Fatalf("waiter value=%+v err=%v", waiter.value, waiter.err)
	}
	for _, secret := range []string{panicValue, key.SubID, key.EffectiveHost} {
		if strings.Contains(waiter.err.Error(), secret) {
			t.Fatalf("waiter error exposed %q: %v", secret, waiter.err)
		}
	}

	cache.mu.RLock()
	_, flightRetained := cache.flights[key]
	cache.mu.RUnlock()
	if flightRetained {
		t.Fatal("panicked flight registration was retained")
	}

	retryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	retry, err := cache.Refresh(retryCtx, key, func(context.Context) ([]string, error) {
		return []string{"vless://retry"}, nil
	})
	if err != nil || strings.Join(retry.Links, ",") != "vless://retry" {
		t.Fatalf("retry value=%+v err=%v", retry, err)
	}
}

func TestLinkCachePruneServerIsLocal(t *testing.T) {
	cache := newLinkCache(2)
	keep := linkKey{ServerID: 1, SubID: "keep", EffectiveHost: "host"}
	removed := linkKey{ServerID: 1, SubID: "removed", EffectiveHost: "host"}
	otherHost := linkKey{ServerID: 1, SubID: "keep", EffectiveHost: "old-host"}
	serverTwoA := linkKey{ServerID: 2, SubID: "a", EffectiveHost: "host"}
	serverTwoB := linkKey{ServerID: 2, SubID: "b", EffectiveHost: "host"}
	for _, key := range []linkKey{keep, removed, otherHost, serverTwoA, serverTwoB} {
		mustRefreshLinkCache(t, cache, key, []string{"vless://" + key.SubID})
	}

	cache.PruneServer(1, map[linkKey]struct{}{keep: {}})
	if _, ok := cache.Get(keep); !ok {
		t.Fatal("kept server-1 key was pruned")
	}
	for _, key := range []linkKey{removed, otherHost} {
		if _, ok := cache.Get(key); ok {
			t.Fatalf("omitted server-1 key retained: %+v", key)
		}
	}
	for _, key := range []linkKey{serverTwoA, serverTwoB} {
		if _, ok := cache.Get(key); !ok {
			t.Fatalf("server-2 key was pruned: %+v", key)
		}
	}
}

func TestLinkCachePruneInvalidatesObsoleteFlight(t *testing.T) {
	cache := newLinkCache(2)
	key := linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "host"}
	oldStarted := make(chan struct{})
	oldRelease := make(chan struct{})
	oldDone := make(chan error, 1)
	go func() {
		_, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
			close(oldStarted)
			<-oldRelease
			return []string{"vless://old"}, nil
		})
		oldDone <- err
	}()
	<-oldStarted

	cache.PruneServer(1, map[linkKey]struct{}{})
	newDone := make(chan error, 1)
	go func() {
		_, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
			return []string{"vless://new"}, nil
		})
		newDone <- err
	}()
	select {
	case err := <-newDone:
		if err != nil {
			t.Fatalf("new refresh err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("new refresh joined obsolete flight")
	}

	close(oldRelease)
	if err := <-oldDone; err != nil {
		t.Fatalf("old refresh err=%v", err)
	}
	value, ok := cache.Get(key)
	if !ok || strings.Join(value.Links, ",") != "vless://new" {
		t.Fatalf("obsolete flight repopulated cache: value=%+v ok=%v", value, ok)
	}
}

func TestLinkCacheNonPositiveLimitStillRuns(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(string(rune('0'-limit)), func(t *testing.T) {
			cache := newLinkCache(limit)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			value, err := cache.Refresh(ctx, linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "host"}, func(context.Context) ([]string, error) {
				return []string{"vless://ok"}, nil
			})
			if err != nil || len(value.Links) != 1 {
				t.Fatalf("value=%+v err=%v", value, err)
			}
		})
	}
}

func mustRefreshLinkCache(t *testing.T, cache *linkCache, key linkKey, links []string) linkValue {
	t.Helper()
	value, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) {
		return links, nil
	})
	if err != nil {
		t.Fatalf("refresh %+v: %v", key, err)
	}
	return value
}
