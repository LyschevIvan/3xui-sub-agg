package aggregator

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

var (
	errInvalidMutation       = errors.New("invalid client mutation")
	errMutationInventory     = errors.New("client inventory unavailable")
	errMutationRequestFailed = errors.New("panel mutation failed")
	errUnsupportedInbound    = errors.New("client creation requires a VLESS inbound")
)

type MutationResult struct {
	Attempted int
	Succeeded int
	Noop      bool
}

type mutationKey struct {
	ServerID int64
	SubID    string
}

type mutationGate struct {
	ready chan struct{}
	refs  int
}

func (a *Aggregator) AttachGroup(
	ctx context.Context,
	userID, serverID int64,
	subID string,
	inboundID int,
) (MutationResult, error) {
	if userID <= 0 || serverID <= 0 || subID == "" || inboundID <= 0 {
		return MutationResult{}, errInvalidMutation
	}
	release, err := a.acquireMutationGate(ctx, mutationKey{ServerID: serverID, SubID: subID})
	if err != nil {
		return MutationResult{}, err
	}
	defer release()
	sc, err := a.ownedMutationClient(userID, serverID)
	if err != nil {
		return MutationResult{}, err
	}
	clients, err := a.freshMutationClients(ctx, sc)
	if err != nil {
		return MutationResult{}, err
	}

	exact := exactClientSummaries(clients, subID)
	for _, client := range exact {
		if containsInbound(client.InboundIDs, inboundID) {
			result := MutationResult{Noop: true}
			a.refresh(ctx)
			return result, nil
		}
	}

	if len(exact) == 0 {
		return a.createGroupClient(ctx, sc, clients, subID, inboundID)
	}

	for i := range exact {
		if exact[i].RecordID != nil && *exact[i].RecordID > 0 {
			continue
		}
		detail, detailErr := a.freshClientDetail(ctx, sc, exact[i].Email)
		if detailErr != nil {
			return MutationResult{}, detailErr
		}
		if detail.Client.RecordID > 0 {
			exact[i].RecordID = intPointer(detail.Client.RecordID)
		}
	}
	selected := canonicalAttachClient(exact)
	result := MutationResult{Attempted: 1}
	err = a.runMutationPOST(ctx, sc, func(callCtx context.Context) error {
		return sc.api.AttachClient(callCtx, selected.Email, []int{inboundID})
	})
	if err == nil {
		result.Succeeded = 1
	}
	a.refresh(ctx)
	return result, err
}

func (a *Aggregator) DetachGroup(
	ctx context.Context,
	userID, serverID int64,
	subID string,
	inboundID int,
) (MutationResult, error) {
	if userID <= 0 || serverID <= 0 || subID == "" || inboundID <= 0 {
		return MutationResult{}, errInvalidMutation
	}
	release, err := a.acquireMutationGate(ctx, mutationKey{ServerID: serverID, SubID: subID})
	if err != nil {
		return MutationResult{}, err
	}
	defer release()
	sc, err := a.ownedMutationClient(userID, serverID)
	if err != nil {
		return MutationResult{}, err
	}
	clients, err := a.freshMutationClients(ctx, sc)
	if err != nil {
		return MutationResult{}, err
	}

	emailSet := make(map[string]struct{})
	for _, client := range clients {
		if client.SubID == subID && containsInbound(client.InboundIDs, inboundID) {
			emailSet[client.Email] = struct{}{}
		}
	}
	emails := make([]string, 0, len(emailSet))
	for email := range emailSet {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	if len(emails) == 0 {
		result := MutationResult{Noop: true}
		a.refresh(ctx)
		return result, nil
	}

	result := MutationResult{}
	var failures int
	var staleErr error
	for _, email := range emails {
		postAttempted, postErr := a.runMutationPOSTIfCurrent(ctx, sc, func(callCtx context.Context) error {
			return sc.api.DetachClient(callCtx, email, []int{inboundID})
		})
		if postAttempted {
			result.Attempted++
		}
		if postErr == nil {
			result.Succeeded++
			continue
		}
		failures++
		if errors.Is(postErr, errStaleConnection) {
			staleErr = errStaleConnection
		}
	}
	a.refresh(ctx)
	if staleErr != nil {
		return result, staleErr
	}
	if failures > 0 {
		return result, fmt.Errorf("%w (%d of %d failed)", errMutationRequestFailed, failures, len(emails))
	}
	return result, nil
}

// acquireMutationGate serializes the fresh-inventory decision and mutation
// POST for one exact group on one server. The gate has its own mutex and never
// holds Aggregator.mu while waiting or performing panel I/O.
func (a *Aggregator) acquireMutationGate(ctx context.Context, key mutationKey) (func(), error) {
	a.mutationMu.Lock()
	if a.mutationGates == nil {
		a.mutationGates = make(map[mutationKey]*mutationGate)
	}
	gate := a.mutationGates[key]
	if gate == nil {
		gate = &mutationGate{ready: make(chan struct{}, 1)}
		gate.ready <- struct{}{}
		a.mutationGates[key] = gate
	}
	gate.refs++
	a.mutationMu.Unlock()

	select {
	case <-ctx.Done():
		a.dropMutationGateRef(key, gate)
		return nil, ctx.Err()
	case <-gate.ready:
		if err := ctx.Err(); err != nil {
			gate.ready <- struct{}{}
			a.dropMutationGateRef(key, gate)
			return nil, err
		}
		return func() {
			gate.ready <- struct{}{}
			a.dropMutationGateRef(key, gate)
		}, nil
	}
}

func (a *Aggregator) dropMutationGateRef(key mutationKey, gate *mutationGate) {
	a.mutationMu.Lock()
	gate.refs--
	if gate.refs == 0 && a.mutationGates[key] == gate {
		delete(a.mutationGates, key)
	}
	a.mutationMu.Unlock()
}

func (a *Aggregator) ownedMutationClient(userID, serverID int64) (*serverClient, error) {
	server, err := a.store.ServerByID(userID, serverID)
	if err != nil {
		return nil, err
	}
	return a.clientFor(*server)
}

func (a *Aggregator) freshMutationClients(ctx context.Context, sc *serverClient) ([]xui.ClientSummary, error) {
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	clients, err := sc.api.ListClients(callCtx)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return nil, currentErr
	}
	if err != nil {
		return nil, errMutationInventory
	}
	return clients, nil
}

func (a *Aggregator) freshClientDetail(ctx context.Context, sc *serverClient, email string) (xui.ClientDetail, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return xui.ClientDetail{}, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	detail, err := sc.api.GetClient(callCtx, email)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return xui.ClientDetail{}, currentErr
	}
	if err != nil {
		return xui.ClientDetail{}, errMutationInventory
	}
	return detail, nil
}

func (a *Aggregator) createGroupClient(
	ctx context.Context,
	sc *serverClient,
	clients []xui.ClientSummary,
	subID string,
	inboundID int,
) (MutationResult, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return MutationResult{}, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	document, err := sc.api.GetInbound(callCtx, inboundID)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return MutationResult{}, currentErr
	}
	if err != nil {
		return MutationResult{}, errMutationInventory
	}
	protocol, err := document.String("protocol")
	if err != nil {
		return MutationResult{}, errMutationInventory
	}
	if !strings.EqualFold(protocol, "vless") {
		return MutationResult{}, errUnsupportedInbound
	}
	remark, err := document.String("remark")
	if err != nil {
		return MutationResult{}, errMutationInventory
	}
	network, security := (xui.InboundSummary{StreamSettings: document["streamSettings"]}).NetworkSecurity()
	uuid, err := randomUUIDv4()
	if err != nil {
		return MutationResult{}, errMutationRequestFailed
	}
	payload := xui.ClientPayload{
		ID:       uuid,
		Security: "auto",
		Email:    uniqueClientEmail(clients, subID, remark, inboundID),
		Enable:   true,
		SubID:    subID,
	}
	if strings.EqualFold(network, "tcp") && strings.EqualFold(security, "reality") {
		payload.Flow = "xtls-rprx-vision"
	}
	result := MutationResult{Attempted: 1}
	err = a.runMutationPOST(ctx, sc, func(callCtx context.Context) error {
		return sc.api.AddClient(callCtx, payload, []int{inboundID})
	})
	if err == nil {
		result.Succeeded = 1
	}
	a.refresh(ctx)
	return result, err
}

func (a *Aggregator) runMutationPOST(ctx context.Context, sc *serverClient, post func(context.Context) error) error {
	_, err := a.runMutationPOSTIfCurrent(ctx, sc, post)
	return err
}

func (a *Aggregator) runMutationPOSTIfCurrent(
	ctx context.Context,
	sc *serverClient,
	post func(context.Context) error,
) (bool, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return false, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	err := post(callCtx)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return true, currentErr
	}
	if err != nil {
		return true, errMutationRequestFailed
	}
	return true, nil
}

func (a *Aggregator) ensureMutationGeneration(sc *serverClient) error {
	if sc == nil || !a.currentClient(sc) {
		return errStaleConnection
	}
	if _, err := a.authoritativeServer(sc.srv); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return errStaleConnection
		}
		return err
	}
	if !a.currentClient(sc) {
		return errStaleConnection
	}
	return nil
}

func exactClientSummaries(clients []xui.ClientSummary, subID string) []xui.ClientSummary {
	exact := make([]xui.ClientSummary, 0)
	for _, client := range clients {
		if client.SubID == subID {
			exact = append(exact, client)
		}
	}
	return exact
}

func canonicalAttachClient(clients []xui.ClientSummary) xui.ClientSummary {
	sort.SliceStable(clients, func(i, j int) bool {
		iHasID := clients[i].RecordID != nil && *clients[i].RecordID > 0
		jHasID := clients[j].RecordID != nil && *clients[j].RecordID > 0
		if iHasID != jHasID {
			return iHasID
		}
		if iHasID && *clients[i].RecordID != *clients[j].RecordID {
			return *clients[i].RecordID < *clients[j].RecordID
		}
		return clients[i].Email < clients[j].Email
	})
	return clients[0]
}

func containsInbound(inbounds []int, target int) bool {
	for _, inboundID := range inbounds {
		if inboundID == target {
			return true
		}
	}
	return false
}

func intPointer(value int) *int {
	return &value
}

func uniqueClientEmail(clients []xui.ClientSummary, subID, remark string, inboundID int) string {
	name := strings.TrimSpace(remark)
	if name == "" {
		name = fmt.Sprintf("inbound-%d", inboundID)
	}
	base := sanitizeClientEmail(subID + "-" + name)
	used := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		used[client.Email] = struct{}{}
	}
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func sanitizeClientEmail(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, char := range value {
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9',
			char == '.', char == '_', char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

func randomUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}
