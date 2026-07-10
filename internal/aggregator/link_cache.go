package aggregator

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

type linkKey struct {
	ServerID      int64
	SubID         string
	EffectiveHost string
}

type linkValue struct {
	Links     []string
	FetchedAt time.Time
}

type linkFlight struct {
	done     chan struct{}
	value    linkValue
	err      error
	obsolete bool
}

type linkCache struct {
	mu      sync.RWMutex
	values  map[linkKey]linkValue
	flights map[linkKey]*linkFlight
	slots   chan struct{}
}

type fetchLinks func(context.Context) ([]string, error)

var errLinkFetchPanic = errors.New("subscription links: fetch failed")

func newLinkCache(limit int) *linkCache {
	if limit <= 0 {
		limit = 1
	}
	return &linkCache{
		values:  make(map[linkKey]linkValue),
		flights: make(map[linkKey]*linkFlight),
		slots:   make(chan struct{}, limit),
	}
}

func (c *linkCache) Get(key linkKey) (linkValue, bool) {
	c.mu.RLock()
	value, ok := c.values[key]
	value = cloneLinkValue(value)
	c.mu.RUnlock()
	return value, ok
}

func (c *linkCache) GetOrFetch(ctx context.Context, key linkKey, fetch fetchLinks) (linkValue, error) {
	return c.fetch(ctx, key, fetch, true)
}

func (c *linkCache) Refresh(ctx context.Context, key linkKey, fetch fetchLinks) (linkValue, error) {
	return c.fetch(ctx, key, fetch, false)
}

func (c *linkCache) fetch(ctx context.Context, key linkKey, fetch fetchLinks, useCached bool) (linkValue, error) {
	c.mu.Lock()
	if useCached {
		if value, ok := c.values[key]; ok {
			c.mu.Unlock()
			return cloneLinkValue(value), nil
		}
	}
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		return waitLinkFlight(ctx, flight)
	}

	flight := &linkFlight{done: make(chan struct{})}
	c.flights[key] = flight
	stale, hasStale := c.values[key]
	stale = cloneLinkValue(stale)
	c.mu.Unlock()

	return c.runLinkFlight(ctx, key, flight, fetch, stale, hasStale)
}

func (c *linkCache) runLinkFlight(
	ctx context.Context,
	key linkKey,
	flight *linkFlight,
	fetch fetchLinks,
	stale linkValue,
	hasStale bool,
) (value linkValue, err error) {
	var panicValue any
	defer func() {
		if recovered := recover(); recovered != nil {
			panicValue = recovered
			if hasStale {
				value = cloneLinkValue(stale)
			} else {
				value = linkValue{}
			}
			err = errLinkFetchPanic
		}
		c.finishLinkFlight(key, flight, value, err)
		if panicValue != nil {
			panic(panicValue)
		}
	}()

	return c.runFetch(ctx, fetch, stale, hasStale)
}

func (c *linkCache) finishLinkFlight(key linkKey, flight *linkFlight, value linkValue, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	flight.value = cloneLinkValue(value)
	flight.err = err
	if current, ok := c.flights[key]; ok && current == flight {
		if err == nil && !flight.obsolete {
			c.values[key] = cloneLinkValue(value)
		}
		delete(c.flights, key)
	}
	close(flight.done)
}

func (c *linkCache) runFetch(ctx context.Context, fetch fetchLinks, stale linkValue, hasStale bool) (linkValue, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		if hasStale {
			return cloneLinkValue(stale), ctx.Err()
		}
		return linkValue{}, ctx.Err()
	}

	links, err := fetch(ctx)
	if err != nil {
		if hasStale {
			return cloneLinkValue(stale), err
		}
		return linkValue{}, err
	}
	return linkValue{Links: normalizeLinks(links), FetchedAt: time.Now()}, nil
}

func waitLinkFlight(ctx context.Context, flight *linkFlight) (linkValue, error) {
	select {
	case <-flight.done:
		return cloneLinkValue(flight.value), flight.err
	case <-ctx.Done():
		return linkValue{}, ctx.Err()
	}
}

func (c *linkCache) PruneServer(serverID int64, keep map[linkKey]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.values {
		if key.ServerID != serverID {
			continue
		}
		if _, ok := keep[key]; !ok {
			delete(c.values, key)
		}
	}
	for key, flight := range c.flights {
		if key.ServerID != serverID {
			continue
		}
		if _, ok := keep[key]; !ok {
			flight.obsolete = true
			delete(c.flights, key)
		}
	}
}

func normalizeLinks(links []string) []string {
	normalized := make([]string, 0, len(links))
	for _, link := range links {
		if strings.TrimSpace(link) == "" {
			continue
		}
		normalized = append(normalized, link)
	}
	sort.Strings(normalized)

	write := 0
	for _, link := range normalized {
		if write > 0 && link == normalized[write-1] {
			continue
		}
		normalized[write] = link
		write++
	}
	return normalized[:write]
}

func cloneLinkValue(value linkValue) linkValue {
	value.Links = append([]string(nil), value.Links...)
	return value
}
