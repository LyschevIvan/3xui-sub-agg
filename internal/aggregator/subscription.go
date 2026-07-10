package aggregator

import (
	"context"
	"errors"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

var (
	ErrSubscriptionNotFound    = errors.New("subscription not found")
	ErrSubscriptionUnavailable = errors.New("subscription temporarily unavailable")
	errDiscoveryFailed         = errors.New("subscription discovery failed")
)

type discoveryKey struct {
	ServerID int64
	Epoch    uint64
	SubID    string
}

type discoveryFlight struct {
	done    chan struct{}
	present bool
	err     error
	waiters int
}

type SubscriptionResult struct {
	Links   []string
	Partial bool
}

// ResolveSubscription returns native links for an exact SubID on servers owned
// by userID. It intentionally exposes only the two fixed subscription errors.
func (a *Aggregator) ResolveSubscription(ctx context.Context, userID int64, subID string) (SubscriptionResult, error) {
	if subID == "" {
		return SubscriptionResult{}, ErrSubscriptionNotFound
	}

	servers, err := a.store.ListServersByUser(userID)
	if err != nil {
		return SubscriptionResult{}, ErrSubscriptionUnavailable
	}
	return a.resolveSubscriptionServers(ctx, userID, subID, servers)
}

func (a *Aggregator) resolveSubscriptionServers(
	ctx context.Context,
	userID int64,
	subID string,
	servers []storage.Server,
) (SubscriptionResult, error) {
	snapshotByID := make(map[int64]ServerSnapshot)
	if snapshot := a.snap.Load(); snapshot != nil {
		for _, server := range snapshot.Servers {
			if server.UserID == userID {
				snapshotByID[server.ID] = server
			}
		}
	}

	var allLinks []string
	failed := false
	for _, preloaded := range servers {
		server, authoritativeErr := a.authoritativeServer(preloaded)
		if authoritativeErr != nil {
			failed = true
			continue
		}
		snapshot, hasSnapshot := snapshotByID[server.ID]
		currentEpoch := a.serverEpoch(server.ID)
		key := linkKey{
			ServerID: server.ID, Epoch: a.cachedEpoch(server),
			SubID: subID, EffectiveHost: publicHost(server),
		}

		completedCurrentInventory := hasSnapshot && !snapshot.FetchedAt.IsZero() &&
			(snapshot.Epoch == 0 || snapshot.Epoch == currentEpoch)
		if completedCurrentInventory {
			if _, present := snapshot.Groups[subID]; !present {
				// A completed inventory is authoritative even when the most
				// recent attempt failed and retained that inventory.
				continue
			}
			failed = failed || snapshot.State != ServerOK
			links, fetchFailed := a.resolveServerLinks(ctx, server, key)
			allLinks = append(allLinks, links...)
			failed = failed || fetchFailed
			continue
		}

		// A cached value proves a prior exact discovery and avoids any panel
		// call while the initial full inventory is still unavailable.
		if cached, ok := a.links.Get(key); ok {
			allLinks = append(allLinks, cached.Links...)
			failed = failed || (hasSnapshot && snapshot.State != ServerOK)
			continue
		}

		apiClient, clientErr := a.clientFor(server)
		if clientErr != nil {
			failed = true
			continue
		}
		present, discoveryErr := a.discoverSubscription(ctx, apiClient, subID)
		if discoveryErr != nil {
			failed = true
			continue
		}
		if !present {
			continue
		}

		key.Epoch = apiClient.epoch
		links, fetchFailed := a.resolveServerLinksWithClient(ctx, key, apiClient)
		allLinks = append(allLinks, links...)
		failed = failed || fetchFailed
	}

	allLinks = normalizeLinks(allLinks)
	if len(allLinks) > 0 {
		return SubscriptionResult{Links: allLinks, Partial: failed}, nil
	}
	if failed {
		return SubscriptionResult{}, ErrSubscriptionUnavailable
	}
	return SubscriptionResult{}, ErrSubscriptionNotFound
}

func containsExactSubID(clients []xui.ClientSummary, subID string) bool {
	for _, client := range clients {
		if client.SubID == subID {
			return true
		}
	}
	return false
}

func (a *Aggregator) resolveServerLinks(ctx context.Context, server storage.Server, key linkKey) ([]string, bool) {
	if cached, ok := a.links.Get(key); ok {
		return cached.Links, false
	}
	apiClient, err := a.clientFor(server)
	if err != nil {
		return nil, true
	}
	key.Epoch = apiClient.epoch
	if cached, ok := a.links.Get(key); ok {
		return cached.Links, false
	}
	return a.resolveServerLinksWithClient(ctx, key, apiClient)
}

func (a *Aggregator) resolveServerLinksWithClient(ctx context.Context, key linkKey, sc *serverClient) ([]string, bool) {
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	defer cancel()
	value, err := a.links.GetOrFetch(callCtx, key, func(fetchCtx context.Context) ([]string, error) {
		if !a.currentClient(sc) {
			return nil, errStaleConnection
		}
		links, err := sc.api.SubLinks(fetchCtx, key.SubID, key.EffectiveHost)
		if err != nil {
			return nil, err
		}
		if !a.currentClient(sc) {
			return nil, errStaleConnection
		}
		return links, nil
	})
	return value.Links, err != nil
}

func (a *Aggregator) discoverSubscription(ctx context.Context, sc *serverClient, subID string) (bool, error) {
	if !a.currentClient(sc) {
		return false, errStaleConnection
	}
	key := discoveryKey{ServerID: sc.srv.ID, Epoch: sc.epoch, SubID: subID}
	a.mu.Lock()
	if flight, ok := a.discoveries[key]; ok {
		flight.waiters++
		a.mu.Unlock()
		return waitDiscoveryFlight(ctx, sc, flight)
	}
	flight := &discoveryFlight{done: make(chan struct{})}
	a.discoveries[key] = flight
	a.mu.Unlock()
	return a.runDiscoveryFlight(ctx, sc, key, flight, subID)
}

func (a *Aggregator) runDiscoveryFlight(
	ctx context.Context,
	sc *serverClient,
	key discoveryKey,
	flight *discoveryFlight,
	subID string,
) (present bool, err error) {
	defer func() {
		if recover() != nil {
			present = false
			err = errDiscoveryFailed
		}
		a.mu.Lock()
		flight.present = present
		flight.err = err
		if current, ok := a.discoveries[key]; ok && current == flight {
			delete(a.discoveries, key)
		}
		close(flight.done)
		a.mu.Unlock()
	}()

	if !a.currentClient(sc) {
		return false, errStaleConnection
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	defer cancel()
	clients, err := sc.api.ListClients(callCtx)
	if err != nil {
		return false, err
	}
	if !a.currentClient(sc) {
		return false, errStaleConnection
	}
	return containsExactSubID(clients, subID), nil
}

func waitDiscoveryFlight(ctx context.Context, sc *serverClient, flight *discoveryFlight) (bool, error) {
	select {
	case <-flight.done:
		return flight.present, flight.err
	case <-sc.ctx.Done():
		return false, errStaleConnection
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (a *Aggregator) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}
