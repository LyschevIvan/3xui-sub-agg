package aggregator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type fetchAPI interface {
	Validate(context.Context) (xui.ServerStatus, error)
	ListClients(context.Context) ([]xui.ClientSummary, error)
	ListSlimInbounds(context.Context) ([]xui.InboundSummary, error)
	SubLinks(context.Context, string, string) ([]string, error)
}

type nativeFetcher struct {
	links   *linkCache
	workers int
}

func (f *nativeFetcher) Fetch(ctx context.Context, srv storage.Server, api fetchAPI, effectiveHost string) (ServerSnapshot, error) {
	snapshot := ServerSnapshot{
		ID:          srv.ID,
		UserID:      srv.UserID,
		Name:        srv.Name,
		PublicHost:  effectiveHost,
		AttemptedAt: time.Now(),
	}

	status, err := api.Validate(ctx)
	if err != nil {
		return snapshot, fmt.Errorf("validate server: %w", err)
	}
	snapshot.PanelVersion = status.PanelVersion

	clients, err := api.ListClients(ctx)
	if err != nil {
		return snapshot, fmt.Errorf("list clients: %w", err)
	}
	inbounds, err := api.ListSlimInbounds(ctx)
	if err != nil {
		return snapshot, fmt.Errorf("list inbounds: %w", err)
	}

	snapshot.Groups = groupClients(clients)
	infos, inboundsEnabled := nativeInboundInfo(inbounds)
	snapshot.Inbounds = infos

	active := make([]linkKey, 0, len(snapshot.Groups))
	keep := make(map[linkKey]struct{}, len(snapshot.Groups))
	for subID, group := range snapshot.Groups {
		if !clientGroupActive(group, inboundsEnabled) {
			continue
		}
		key := linkKey{ServerID: srv.ID, SubID: subID, EffectiveHost: effectiveHost}
		active = append(active, key)
		keep[key] = struct{}{}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].SubID < active[j].SubID })

	// Pruning is safe only after validation and both inventory reads have
	// completed. The keep-set intentionally contains active keys for this exact
	// effective host and no stale host or inactive group keys.
	f.links.PruneServer(srv.ID, keep)

	syncErr := f.refreshLinks(ctx, api, active)
	snapshot.State = ServerOK
	if syncErr != nil {
		snapshot.State = ServerDegraded
		snapshot.SyncErr = syncErr
	}
	snapshot.FetchedAt = time.Now()
	return snapshot, nil
}

func nativeInboundInfo(inbounds []xui.InboundSummary) ([]InboundInfo, map[int]struct{}) {
	infos := make([]InboundInfo, 0, len(inbounds))
	enabled := make(map[int]struct{}, len(inbounds))
	for _, inbound := range inbounds {
		network, security := inbound.NetworkSecurity()
		infos = append(infos, InboundInfo{
			ID:       inbound.ID,
			Remark:   inbound.Remark,
			Port:     inbound.Port,
			Protocol: inbound.Protocol,
			Network:  network,
			Security: security,
			Enable:   inbound.Enable,
		})
		if inbound.Enable {
			enabled[inbound.ID] = struct{}{}
		}
	}
	return infos, enabled
}

func clientGroupActive(group ClientGroup, enabledInbounds map[int]struct{}) bool {
	for _, record := range group.Records {
		if !record.Enabled {
			continue
		}
		for _, inboundID := range record.InboundIDs {
			if _, ok := enabledInbounds[inboundID]; ok {
				return true
			}
		}
	}
	return false
}

func (f *nativeFetcher) refreshLinks(ctx context.Context, api fetchAPI, keys []linkKey) error {
	if len(keys) == 0 {
		return nil
	}
	workers := f.workers
	if workers <= 0 {
		workers = 1
	}

	type refreshJob struct {
		index int
		key   linkKey
	}
	jobs := make(chan refreshJob, workers)
	var wg sync.WaitGroup
	syncErrors := make([]error, len(keys))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				key := job.key
				_, err := f.links.Refresh(ctx, key, func(ctx context.Context) ([]string, error) {
					return api.SubLinks(ctx, key.SubID, key.EffectiveHost)
				})
				if err != nil {
					syncErrors[job.index] = err
				}
			}
		}()
	}

sendJobs:
	for index, key := range keys {
		select {
		case jobs <- refreshJob{index: index, key: key}:
		case <-ctx.Done():
			syncErrors[index] = ctx.Err()
			break sendJobs
		}
	}
	close(jobs)
	wg.Wait()

	ordered := make([]error, 0, len(syncErrors))
	for _, err := range syncErrors {
		if err != nil {
			ordered = append(ordered, err)
		}
	}
	return errors.Join(ordered...)
}
