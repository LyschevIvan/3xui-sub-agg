package aggregator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

var (
	errInvalidInboundMutation = errors.New("invalid inbound mutation")
	errInboundInventory       = errors.New("inbound inventory unavailable")
	errInboundRequestFailed   = errors.New("panel inbound mutation failed")
	errCopyCompensation       = errors.New("inbound copy compensation failed")
)

type InboundPatch struct {
	Remark string
	Port   int
	Enable bool
}

type CopyInboundRequest struct {
	UserID          int64
	SourceServerID  int64
	SourceInboundID int
	TargetServerID  int64
	Remark          string
	Port            int
}

type inboundMutationKey struct {
	ServerID  int64
	InboundID int
}

type copyClient struct {
	SubID   string
	Payload xui.ClientPayload
}

type createdCopyClient struct {
	SubID string
	Email string
}

func (a *Aggregator) EditInbound(
	ctx context.Context,
	userID, serverID int64,
	inboundID int,
	patch InboundPatch,
) error {
	if userID <= 0 || serverID <= 0 || inboundID <= 0 || !validInboundPort(patch.Port) {
		return errInvalidInboundMutation
	}
	release, err := a.acquireInboundMutationGate(ctx, inboundMutationKey{ServerID: serverID, InboundID: inboundID})
	if err != nil {
		return err
	}
	defer release()

	sc, err := a.ownedMutationClient(userID, serverID)
	if err != nil {
		return err
	}
	document, err := a.freshInboundDocument(ctx, sc, inboundID)
	if err != nil {
		return err
	}
	if err := requireVLESS(document); err != nil {
		return err
	}
	updated := document.Clone()
	if err := setInboundEditFields(updated, patch); err != nil {
		return errInboundInventory
	}
	err = a.runMutationPOST(ctx, sc, func(callCtx context.Context) error {
		_, updateErr := sc.api.UpdateInbound(callCtx, inboundID, updated)
		return updateErr
	})
	a.refresh(ctx)
	return err
}

func (a *Aggregator) DeleteInbound(ctx context.Context, userID, serverID int64, inboundID int) error {
	if userID <= 0 || serverID <= 0 || inboundID <= 0 {
		return errInvalidInboundMutation
	}
	release, err := a.acquireInboundMutationGate(ctx, inboundMutationKey{ServerID: serverID, InboundID: inboundID})
	if err != nil {
		return err
	}
	defer release()

	sc, err := a.ownedMutationClient(userID, serverID)
	if err != nil {
		return err
	}
	err = a.runMutationPOST(ctx, sc, func(callCtx context.Context) error {
		return sc.api.DeleteInbound(callCtx, inboundID)
	})
	a.refresh(ctx)
	return err
}

func (a *Aggregator) CopyInbound(ctx context.Context, req CopyInboundRequest) (int, error) {
	if req.UserID <= 0 || req.SourceServerID <= 0 || req.TargetServerID <= 0 ||
		req.SourceInboundID <= 0 || !validInboundPort(req.Port) {
		return 0, errInvalidInboundMutation
	}
	releaseSource, err := a.acquireInboundMutationGate(ctx, inboundMutationKey{
		ServerID: req.SourceServerID, InboundID: req.SourceInboundID,
	})
	if err != nil {
		return 0, err
	}
	defer releaseSource()

	// Both ownership checks happen before the first panel read or mutation.
	sourceSC, err := a.ownedMutationClient(req.UserID, req.SourceServerID)
	if err != nil {
		return 0, err
	}
	targetSC, err := a.ownedMutationClient(req.UserID, req.TargetServerID)
	if err != nil {
		return 0, err
	}

	sourceDocument, err := a.freshInboundDocument(ctx, sourceSC, req.SourceInboundID)
	if err != nil {
		return 0, err
	}
	if err := requireVLESS(sourceDocument); err != nil {
		return 0, err
	}
	if req.SourceServerID == req.TargetServerID {
		sourcePort, portErr := sourceDocument.Int("port")
		if portErr != nil {
			return 0, errInboundInventory
		}
		if sourcePort == req.Port {
			return 0, errInvalidInboundMutation
		}
	}

	copyClients, err := a.freshCopyClients(ctx, sourceSC, sourceDocument, req.SourceInboundID)
	if err != nil {
		return 0, err
	}
	clone := sourceDocument.Clone()
	for _, key := range []string{"id", "up", "down", "clientStats"} {
		clone.Delete(key)
	}
	if err := clone.Set("remark", req.Remark); err != nil {
		return 0, errInboundInventory
	}
	if err := clone.Set("port", req.Port); err != nil {
		return 0, errInboundInventory
	}
	if err := clone.Set("tag", fmt.Sprintf("inbound-%d", req.Port)); err != nil {
		return 0, errInboundInventory
	}
	if err := clone.SetClients([]xui.ClientPayload{}); err != nil {
		return 0, errInboundInventory
	}
	if err := a.ensureMutationGeneration(sourceSC); err != nil {
		return 0, err
	}

	createdDocument, err := a.addInboundOnce(ctx, targetSC, clone)
	if err != nil {
		a.refresh(ctx)
		return 0, err
	}
	createdID, err := createdDocument.Int("id")
	if err != nil || createdID <= 0 {
		a.refresh(ctx)
		return 0, errInboundRequestFailed
	}

	createdClients := make([]createdCopyClient, 0, len(copyClients))
	for _, sourceClient := range copyClients {
		created, mutateErr := a.copyGroupToInbound(ctx, req, targetSC, createdID, clone, sourceClient)
		if created.Email != "" {
			createdClients = append(createdClients, created)
		}
		if mutateErr != nil {
			compensationErr := a.compensateInboundCopy(ctx, req, targetSC, createdID, createdClients)
			a.refresh(ctx)
			return 0, errors.Join(mutateErr, compensationErr)
		}
	}
	a.refresh(ctx)
	return createdID, nil
}

func validInboundPort(port int) bool { return port > 0 && port <= 65535 }

func requireVLESS(document xui.InboundDocument) error {
	protocol, err := document.String("protocol")
	if err != nil {
		return errInboundInventory
	}
	if !strings.EqualFold(protocol, "vless") {
		return errUnsupportedInbound
	}
	return nil
}

func setInboundEditFields(document xui.InboundDocument, patch InboundPatch) error {
	if err := document.Set("remark", patch.Remark); err != nil {
		return err
	}
	if err := document.Set("port", patch.Port); err != nil {
		return err
	}
	if err := document.Set("enable", patch.Enable); err != nil {
		return err
	}
	return document.Set("tag", fmt.Sprintf("inbound-%d", patch.Port))
}

func (a *Aggregator) freshInboundDocument(ctx context.Context, sc *serverClient, inboundID int) (xui.InboundDocument, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return nil, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	document, err := sc.api.GetInbound(callCtx, inboundID)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return nil, currentErr
	}
	if err != nil || document == nil {
		return nil, errInboundInventory
	}
	return document, nil
}

func (a *Aggregator) addInboundOnce(ctx context.Context, sc *serverClient, document xui.InboundDocument) (xui.InboundDocument, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return nil, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	created, err := sc.api.AddInbound(callCtx, document)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return nil, currentErr
	}
	if err != nil {
		return nil, errInboundRequestFailed
	}
	return created, nil
}

func (a *Aggregator) freshCopyClients(
	ctx context.Context,
	sc *serverClient,
	document xui.InboundDocument,
	inboundID int,
) ([]copyClient, error) {
	embedded, err := document.Clients()
	if err != nil {
		return nil, errInboundInventory
	}
	wanted := make(map[string]struct{}, len(embedded))
	for _, client := range embedded {
		if client.SubID != "" {
			wanted[client.SubID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	inventory, err := a.freshMutationClients(ctx, sc)
	if err != nil {
		return nil, err
	}
	subIDs := make([]string, 0, len(wanted))
	for subID := range wanted {
		subIDs = append(subIDs, subID)
	}
	sort.Strings(subIDs)
	result := make([]copyClient, 0, len(subIDs))
	for _, subID := range subIDs {
		exact := exactClientSummaries(inventory, subID)
		attached := exact[:0]
		for _, summary := range exact {
			if containsInbound(summary.InboundIDs, inboundID) {
				attached = append(attached, summary)
			}
		}
		if len(attached) == 0 {
			return nil, errInboundInventory
		}
		for i := range attached {
			if attached[i].RecordID != nil && *attached[i].RecordID > 0 {
				continue
			}
			detail, detailErr := a.freshClientDetail(ctx, sc, attached[i].Email)
			if detailErr != nil {
				return nil, detailErr
			}
			if detail.Client.RecordID > 0 {
				attached[i].RecordID = intPointer(detail.Client.RecordID)
			}
		}
		canonical := canonicalAttachClient(attached)
		detail, detailErr := a.freshClientDetail(ctx, sc, canonical.Email)
		if detailErr != nil {
			return nil, detailErr
		}
		payload, payloadErr := detail.Client.Payload()
		if payloadErr != nil {
			return nil, errInboundInventory
		}
		payload.SubID = subID
		result = append(result, copyClient{SubID: subID, Payload: payload})
	}
	return result, nil
}

func (a *Aggregator) copyGroupToInbound(
	ctx context.Context,
	req CopyInboundRequest,
	createdOn *serverClient,
	createdInboundID int,
	createdDocument xui.InboundDocument,
	source copyClient,
) (createdCopyClient, error) {
	key := mutationKey{ServerID: req.TargetServerID, SubID: source.SubID}
	release, err := a.acquireMutationGate(ctx, key)
	if err != nil {
		return createdCopyClient{}, err
	}
	defer release()

	targetSC, err := a.ownedMutationClient(req.UserID, req.TargetServerID)
	if err != nil {
		return createdCopyClient{}, err
	}
	if targetSC != createdOn {
		return createdCopyClient{}, errStaleConnection
	}
	inventory, err := a.freshMutationClients(ctx, targetSC)
	if err != nil {
		return createdCopyClient{}, err
	}
	exact := exactClientSummaries(inventory, source.SubID)
	for _, client := range exact {
		if containsInbound(client.InboundIDs, createdInboundID) {
			return createdCopyClient{}, nil
		}
	}
	if len(exact) > 0 {
		for i := range exact {
			if exact[i].RecordID != nil && *exact[i].RecordID > 0 {
				continue
			}
			detail, detailErr := a.freshClientDetail(ctx, targetSC, exact[i].Email)
			if detailErr != nil {
				return createdCopyClient{}, detailErr
			}
			if detail.Client.RecordID > 0 {
				exact[i].RecordID = intPointer(detail.Client.RecordID)
			}
		}
		selected := canonicalAttachClient(exact)
		return createdCopyClient{}, a.runMutationPOST(ctx, targetSC, func(callCtx context.Context) error {
			return targetSC.api.AttachClient(callCtx, selected.Email, []int{createdInboundID})
		})
	}

	payload := source.Payload
	uuid, err := randomUUIDv4()
	if err != nil {
		return createdCopyClient{}, errInboundRequestFailed
	}
	payload.ID = uuid
	payload.Email = uniqueClientEmail(inventory, source.SubID, req.Remark, createdInboundID)
	payload.SubID = source.SubID
	payload.CreatedAt = 0
	payload.UpdatedAt = 0
	network, security := (xui.InboundSummary{StreamSettings: createdDocument["streamSettings"]}).NetworkSecurity()
	if strings.EqualFold(network, "tcp") && strings.EqualFold(security, "reality") {
		payload.Flow = "xtls-rprx-vision"
	} else {
		payload.Flow = ""
	}
	attempted, postErr := a.runMutationPOSTIfCurrent(ctx, targetSC, func(callCtx context.Context) error {
		return targetSC.api.AddClient(callCtx, payload, []int{createdInboundID})
	})
	created := createdCopyClient{}
	if attempted && postErr == nil {
		// Only a confirmed successful add is operation-created. A rejected or
		// ambiguous request must never authorize deletion of a later record.
		created = createdCopyClient{SubID: source.SubID, Email: payload.Email}
	}
	return created, postErr
}

func (a *Aggregator) compensateInboundCopy(
	ctx context.Context,
	req CopyInboundRequest,
	createdOn *serverClient,
	createdInboundID int,
	createdClients []createdCopyClient,
) error {
	compensationCtx := context.WithoutCancel(ctx)
	var failures []error
	for i := len(createdClients) - 1; i >= 0; i-- {
		created := createdClients[i]
		release, gateErr := a.acquireMutationGate(compensationCtx, mutationKey{ServerID: req.TargetServerID, SubID: created.SubID})
		if gateErr != nil {
			failures = append(failures, errCopyCompensation)
			continue
		}
		targetSC, acquireErr := a.ownedMutationClient(req.UserID, req.TargetServerID)
		if acquireErr != nil || targetSC != createdOn {
			failures = append(failures, errCopyCompensation)
			release()
			continue
		}
		deleteErr := a.runMutationPOST(compensationCtx, targetSC, func(callCtx context.Context) error {
			return targetSC.api.DeleteClient(callCtx, created.Email)
		})
		if deleteErr != nil {
			failures = append(failures, errCopyCompensation)
		}
		release()
	}
	if targetSC, acquireErr := a.ownedMutationClient(req.UserID, req.TargetServerID); acquireErr != nil || targetSC != createdOn {
		failures = append(failures, errCopyCompensation)
	} else if deleteErr := a.runMutationPOST(compensationCtx, targetSC, func(callCtx context.Context) error {
		return targetSC.api.DeleteInbound(callCtx, createdInboundID)
	}); deleteErr != nil {
		failures = append(failures, errCopyCompensation)
	}
	return errors.Join(failures...)
}

func (a *Aggregator) acquireInboundMutationGate(ctx context.Context, key inboundMutationKey) (func(), error) {
	a.inboundMu.Lock()
	if a.inboundGates == nil {
		a.inboundGates = make(map[inboundMutationKey]*mutationGate)
	}
	gate := a.inboundGates[key]
	if gate == nil {
		gate = &mutationGate{ready: make(chan struct{}, 1)}
		gate.ready <- struct{}{}
		a.inboundGates[key] = gate
	}
	gate.refs++
	a.inboundMu.Unlock()

	select {
	case <-ctx.Done():
		a.dropInboundMutationGateRef(key, gate)
		return nil, ctx.Err()
	case <-gate.ready:
		if err := ctx.Err(); err != nil {
			gate.ready <- struct{}{}
			a.dropInboundMutationGateRef(key, gate)
			return nil, err
		}
		return func() {
			gate.ready <- struct{}{}
			a.dropInboundMutationGateRef(key, gate)
		}, nil
	}
}

func (a *Aggregator) dropInboundMutationGateRef(key inboundMutationKey, gate *mutationGate) {
	a.inboundMu.Lock()
	gate.refs--
	if gate.refs == 0 && a.inboundGates[key] == gate {
		delete(a.inboundGates, key)
	}
	a.inboundMu.Unlock()
}
