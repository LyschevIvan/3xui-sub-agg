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
)

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
	for _, server := range servers {
		a.purgeChangedConnection(server)
		snapshot, hasSnapshot := snapshotByID[server.ID]
		key := linkKey{ServerID: server.ID, SubID: subID, EffectiveHost: publicHost(server)}

		if hasSnapshot && !snapshot.FetchedAt.IsZero() {
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
			continue
		}

		apiClient, clientErr := a.clientFor(server)
		if clientErr != nil {
			failed = true
			continue
		}
		discoveryCtx, cancel := a.callContext(ctx)
		clients, discoveryErr := apiClient.api.ListClients(discoveryCtx)
		cancel()
		if discoveryErr != nil {
			failed = true
			continue
		}
		if !containsExactSubID(clients, subID) {
			continue
		}

		links, fetchFailed := a.resolveServerLinksWithAPI(ctx, key, apiClient.api)
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
	return a.resolveServerLinksWithAPI(ctx, key, apiClient.api)
}

func (a *Aggregator) resolveServerLinksWithAPI(ctx context.Context, key linkKey, api xui.PanelAPI) ([]string, bool) {
	callCtx, cancel := a.callContext(ctx)
	defer cancel()
	value, err := a.links.GetOrFetch(callCtx, key, func(fetchCtx context.Context) ([]string, error) {
		return api.SubLinks(fetchCtx, key.SubID, key.EffectiveHost)
	})
	return value.Links, err != nil
}

func (a *Aggregator) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}
