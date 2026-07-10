package aggregator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

var (
	errInvalidInboundMutation = errors.New("invalid inbound mutation")
	errInboundInventory       = errors.New("inbound inventory unavailable")
	errInboundRequestFailed   = errors.New("panel inbound mutation failed")
	errCopyCompensation       = errors.New("inbound copy compensation failed")
	errCreatedInboundUnowned  = errors.New("created inbound ownership could not be verified")
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
	UUID  string
}

type createdInboundOwnership struct {
	ID       int
	Port     int
	Tag      string
	Resource storage.Server
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
	a.refreshAfterInboundMutation()
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
	document, err := a.freshInboundDocument(ctx, sc, inboundID)
	if err != nil {
		return err
	}
	if err := requireVLESS(document); err != nil {
		return err
	}
	err = a.runMutationPOST(ctx, sc, func(callCtx context.Context) error {
		return sc.api.DeleteInbound(callCtx, inboundID)
	})
	a.refreshAfterInboundMutation()
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
	if strings.TrimSpace(req.Remark) == "" {
		sourceRemark, remarkErr := sourceDocument.String("remark")
		if remarkErr != nil {
			return 0, errInboundInventory
		}
		req.Remark = sourceRemark
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

	releaseGroups, err := a.acquireCopyGroupGates(ctx, req.TargetServerID, copyClients)
	if err != nil {
		return 0, err
	}
	defer releaseGroups()

	baseline, err := a.freshInboundSummaries(ctx, targetSC)
	if err != nil {
		return 0, err
	}
	baselineIDs := make(map[int]struct{}, len(baseline))
	for _, inbound := range baseline {
		if inbound.ID > 0 {
			baselineIDs[inbound.ID] = struct{}{}
		}
	}
	if err := a.ensureCopySourceCurrent(ctx, sourceSC); err != nil {
		return 0, err
	}

	createdDocument, addOutcome := a.addInboundOnce(ctx, targetSC, clone)
	if !addOutcome.Confirmed {
		a.refreshAfterInboundMutation()
		if addOutcome.Err != nil {
			return 0, addOutcome.Err
		}
		return 0, errInboundRequestFailed
	}
	createdID, idErr := createdDocument.Int("id")
	if idErr != nil || createdID <= 0 {
		a.refreshAfterInboundMutation()
		return 0, errors.Join(addOutcome.Err, ctx.Err(), errCreatedInboundUnowned)
	}
	if _, existed := baselineIDs[createdID]; existed {
		a.refreshAfterInboundMutation()
		return 0, errors.Join(addOutcome.Err, ctx.Err(), errCreatedInboundUnowned)
	}

	verificationCtx := ctx
	cancelVerification := func() {}
	if addOutcome.Err != nil || ctx.Err() != nil {
		verificationCtx, cancelVerification = a.inboundMaintenanceContext(1)
	}
	ownership, currentTarget, verifyErr := a.verifyCreatedInbound(
		verificationCtx, req, targetSC.srv, createdID, req.Port, fmt.Sprintf("inbound-%d", req.Port), true,
	)
	cancelVerification()
	if verifyErr != nil {
		a.refreshAfterInboundMutation()
		return 0, errors.Join(addOutcome.Err, ctx.Err(), errCreatedInboundUnowned)
	}
	if operationErr := errors.Join(addOutcome.Err, ctx.Err()); operationErr != nil {
		cleanupErr := a.compensateInboundCopy(req, ownership, nil)
		a.refreshAfterInboundMutation()
		return 0, errors.Join(operationErr, cleanupErr)
	}

	createdClients := make([]createdCopyClient, 0, len(copyClients))
	for _, sourceClient := range copyClients {
		created, mutateErr := a.copyGroupToInbound(ctx, sourceSC, currentTarget, ownership, clone, sourceClient, req.Remark)
		if created.Email != "" {
			createdClients = append(createdClients, created)
		}
		if mutateErr != nil {
			compensationErr := a.compensateInboundCopy(req, ownership, createdClients)
			a.refreshAfterInboundMutation()
			return 0, errors.Join(mutateErr, compensationErr)
		}
		if sourceErr := a.ensureCopySourceCurrent(ctx, sourceSC); sourceErr != nil {
			compensationErr := a.compensateInboundCopy(req, ownership, createdClients)
			a.refreshAfterInboundMutation()
			return 0, errors.Join(sourceErr, compensationErr)
		}
	}
	a.refreshAfterInboundMutation()
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
	documentID, idErr := document.Int("id")
	if idErr != nil || documentID <= 0 || documentID != inboundID {
		return nil, errInboundInventory
	}
	return document, nil
}

func (a *Aggregator) addInboundOnce(
	ctx context.Context,
	sc *serverClient,
	document xui.InboundDocument,
) (xui.InboundDocument, mutationPOSTOutcome) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return nil, mutationPOSTOutcome{Err: err}
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	created, err := sc.api.AddInbound(callCtx, document)
	cancel()
	outcome := mutationPOSTOutcome{Attempted: true, Confirmed: err == nil}
	currentErr := a.ensureMutationGeneration(sc)
	requestErr := ctx.Err()
	if err != nil {
		outcome.Err = errors.Join(errInboundRequestFailed, requestErr, currentErr)
		return created, outcome
	}
	outcome.Err = errors.Join(requestErr, currentErr)
	return created, outcome
}

func (a *Aggregator) freshInboundSummaries(ctx context.Context, sc *serverClient) ([]xui.InboundSummary, error) {
	if err := a.ensureMutationGeneration(sc); err != nil {
		return nil, err
	}
	callCtx, cancel := a.connectionCallContext(ctx, sc)
	inbounds, err := sc.api.ListSlimInbounds(callCtx)
	cancel()
	if currentErr := a.ensureMutationGeneration(sc); currentErr != nil {
		return nil, currentErr
	}
	if err != nil {
		return nil, errInboundInventory
	}
	return inbounds, nil
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
		details := make(map[string]xui.ClientDetail, len(attached))
		for i := range attached {
			detail, detailErr := a.freshClientDetail(ctx, sc, attached[i].Email)
			if detailErr != nil {
				return nil, detailErr
			}
			bound, bindErr := bindClientDetail(attached[i], detail, inboundID)
			if bindErr != nil {
				return nil, bindErr
			}
			attached[i] = bound
			details[bound.Email] = detail
		}
		canonical := canonicalAttachClient(attached)
		detail := details[canonical.Email]
		payload, payloadErr := detail.Client.Payload()
		if payloadErr != nil {
			return nil, errInboundInventory
		}
		result = append(result, copyClient{SubID: subID, Payload: payload})
	}
	return result, nil
}

func (a *Aggregator) copyGroupToInbound(
	ctx context.Context,
	sourceSC *serverClient,
	targetSC *serverClient,
	ownership createdInboundOwnership,
	createdDocument xui.InboundDocument,
	source copyClient,
	remark string,
) (createdCopyClient, error) {
	if err := a.ensureCopySourceCurrent(ctx, sourceSC); err != nil {
		return createdCopyClient{}, err
	}
	inventory, err := a.freshMutationClients(ctx, targetSC)
	if err != nil {
		return createdCopyClient{}, err
	}
	exact := exactClientSummaries(inventory, source.SubID)
	if len(exact) > 0 {
		for i := range exact {
			detail, detailErr := a.freshClientDetail(ctx, targetSC, exact[i].Email)
			if detailErr != nil {
				return createdCopyClient{}, detailErr
			}
			bound, bindErr := bindClientDetail(exact[i], detail, 0)
			if bindErr != nil {
				return createdCopyClient{}, bindErr
			}
			exact[i] = bound
		}
		if err := a.ensureCopySourceCurrent(ctx, sourceSC); err != nil {
			return createdCopyClient{}, err
		}
		for _, client := range exact {
			if containsInbound(client.InboundIDs, ownership.ID) {
				return createdCopyClient{}, nil
			}
		}
		selected := canonicalAttachClient(exact)
		outcome := a.runMutationPOSTOutcome(ctx, targetSC, func(callCtx context.Context) error {
			return targetSC.api.AttachClient(callCtx, selected.Email, []int{ownership.ID})
		})
		return createdCopyClient{}, outcome.Err
	}

	payload := source.Payload
	uuid, err := randomUUIDv4()
	if err != nil {
		return createdCopyClient{}, errInboundRequestFailed
	}
	payload.ID = uuid
	payload.Email = uniqueClientEmail(inventory, source.SubID, remark, ownership.ID)
	payload.SubID = source.SubID
	payload.CreatedAt = 0
	payload.UpdatedAt = 0
	network, security := (xui.InboundSummary{StreamSettings: createdDocument["streamSettings"]}).NetworkSecurity()
	if strings.EqualFold(network, "tcp") && strings.EqualFold(security, "reality") {
		payload.Flow = "xtls-rprx-vision"
	} else {
		payload.Flow = ""
	}
	if err := a.ensureCopySourceCurrent(ctx, sourceSC); err != nil {
		return createdCopyClient{}, err
	}
	outcome := a.runMutationPOSTOutcome(ctx, targetSC, func(callCtx context.Context) error {
		return targetSC.api.AddClient(callCtx, payload, []int{ownership.ID})
	})
	created := createdCopyClient{}
	if outcome.Confirmed {
		created = createdCopyClient{SubID: source.SubID, Email: payload.Email, UUID: payload.ID}
	}
	return created, outcome.Err
}

func bindClientDetail(summary xui.ClientSummary, detail xui.ClientDetail, requiredInboundID int) (xui.ClientSummary, error) {
	if summary.Email == "" || detail.Client.Email != summary.Email || detail.Client.SubID != summary.SubID {
		return xui.ClientSummary{}, errInboundInventory
	}
	if summary.RecordID != nil && *summary.RecordID > 0 {
		if detail.Client.RecordID != *summary.RecordID {
			return xui.ClientSummary{}, errInboundInventory
		}
	} else {
		if detail.Client.RecordID <= 0 {
			return xui.ClientSummary{}, errInboundInventory
		}
		summary.RecordID = intPointer(detail.Client.RecordID)
	}
	if requiredInboundID > 0 && !containsInbound(detail.InboundIDs, requiredInboundID) {
		return xui.ClientSummary{}, errInboundInventory
	}
	summary.InboundIDs = append([]int(nil), detail.InboundIDs...)
	return summary, nil
}

func (a *Aggregator) ensureCopySourceCurrent(ctx context.Context, sourceSC *serverClient) error {
	return errors.Join(ctx.Err(), a.ensureMutationGeneration(sourceSC))
}

func (a *Aggregator) compensateInboundCopy(
	req CopyInboundRequest,
	ownership createdInboundOwnership,
	createdClients []createdCopyClient,
) error {
	compensationCtx, cancel := a.inboundMaintenanceContext(len(createdClients) + 2)
	defer cancel()
	var failures []error
	for i := len(createdClients) - 1; i >= 0; i-- {
		created := createdClients[i]
		targetSC, acquireErr := a.sameResourceMutationClient(req.UserID, req.TargetServerID, ownership.Resource)
		if acquireErr != nil {
			failures = append(failures, errCopyCompensation)
			continue
		}
		detail, detailErr := a.freshClientDetail(compensationCtx, targetSC, created.Email)
		if detailErr != nil || detail.Client.Email != created.Email || detail.Client.UUID != created.UUID ||
			detail.Client.SubID != created.SubID || !containsInbound(detail.InboundIDs, ownership.ID) ||
			hasInboundOtherThan(detail.InboundIDs, ownership.ID) {
			failures = append(failures, errCopyCompensation)
			continue
		}
		deleteOutcome := a.runMutationPOSTOutcome(compensationCtx, targetSC, func(callCtx context.Context) error {
			return targetSC.api.DeleteClient(callCtx, created.Email)
		})
		if !deleteOutcome.Confirmed {
			failures = append(failures, errCopyCompensation)
		}
	}
	targetSC, acquireErr := a.sameResourceMutationClient(req.UserID, req.TargetServerID, ownership.Resource)
	if acquireErr != nil {
		failures = append(failures, errCopyCompensation)
		return errors.Join(failures...)
	}
	verified, verifyErr := a.freshInboundDocument(compensationCtx, targetSC, ownership.ID)
	if verifyErr != nil || !matchesCreatedInbound(verified, ownership, false) {
		failures = append(failures, errCopyCompensation)
		return errors.Join(failures...)
	}
	deleteOutcome := a.runMutationPOSTOutcome(compensationCtx, targetSC, func(callCtx context.Context) error {
		return targetSC.api.DeleteInbound(callCtx, ownership.ID)
	})
	if !deleteOutcome.Confirmed {
		failures = append(failures, errCopyCompensation)
	}
	return errors.Join(failures...)
}

func hasInboundOtherThan(inboundIDs []int, ownedInboundID int) bool {
	for _, inboundID := range inboundIDs {
		if inboundID != ownedInboundID {
			return true
		}
	}
	return false
}

func (a *Aggregator) verifyCreatedInbound(
	ctx context.Context,
	req CopyInboundRequest,
	resource storage.Server,
	createdID, port int,
	tag string,
	requireEmptyClients bool,
) (createdInboundOwnership, *serverClient, error) {
	targetSC, err := a.sameResourceMutationClient(req.UserID, req.TargetServerID, resource)
	if err != nil {
		return createdInboundOwnership{}, nil, err
	}
	document, err := a.freshInboundDocument(ctx, targetSC, createdID)
	if err != nil {
		return createdInboundOwnership{}, nil, err
	}
	ownership := createdInboundOwnership{ID: createdID, Port: port, Tag: tag, Resource: resource}
	if !matchesCreatedInbound(document, ownership, requireEmptyClients) {
		return createdInboundOwnership{}, nil, errCreatedInboundUnowned
	}
	return ownership, targetSC, nil
}

func matchesCreatedInbound(document xui.InboundDocument, ownership createdInboundOwnership, requireEmptyClients bool) bool {
	id, err := document.Int("id")
	if err != nil || id != ownership.ID {
		return false
	}
	protocol, err := document.String("protocol")
	if err != nil || !strings.EqualFold(protocol, "vless") {
		return false
	}
	port, err := document.Int("port")
	if err != nil || port != ownership.Port {
		return false
	}
	tag, err := document.String("tag")
	if err != nil || tag != ownership.Tag {
		return false
	}
	if requireEmptyClients {
		clients, clientsErr := document.Clients()
		if clientsErr != nil || len(clients) != 0 {
			return false
		}
	}
	return true
}

func (a *Aggregator) sameResourceMutationClient(
	userID, serverID int64,
	resource storage.Server,
) (*serverClient, error) {
	server, err := a.store.ServerByID(userID, serverID)
	if err != nil {
		return nil, errStaleConnection
	}
	if !samePanelResource(*server, resource) {
		return nil, errStaleConnection
	}
	return a.clientFor(*server)
}

func samePanelResource(left, right storage.Server) bool {
	return left.ID == right.ID && left.UserID == right.UserID &&
		left.APIURL == right.APIURL && left.Path == right.Path
}

func (a *Aggregator) acquireCopyGroupGates(
	ctx context.Context,
	serverID int64,
	clients []copyClient,
) (func(), error) {
	subIDs := make([]string, 0, len(clients))
	seen := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		if client.SubID == "" {
			continue
		}
		if _, exists := seen[client.SubID]; exists {
			continue
		}
		seen[client.SubID] = struct{}{}
		subIDs = append(subIDs, client.SubID)
	}
	sort.Strings(subIDs)
	releases := make([]func(), 0, len(subIDs))
	for _, subID := range subIDs {
		release, err := a.acquireMutationGate(ctx, mutationKey{ServerID: serverID, SubID: subID})
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

func (a *Aggregator) inboundMaintenanceContext(steps int) (context.Context, context.CancelFunc) {
	const maxTimeout = 2 * time.Minute
	perStep := a.cfg.RequestTimeout
	if perStep <= 0 {
		perStep = 10 * time.Second
	}
	if steps < 1 {
		steps = 1
	}
	timeout := maxTimeout
	if perStep < maxTimeout {
		maxSteps := int(maxTimeout / perStep)
		if steps <= maxSteps {
			timeout = perStep * time.Duration(steps)
		}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (a *Aggregator) refreshAfterInboundMutation() {
	ctx, cancel := a.inboundMaintenanceContext(1)
	defer cancel()
	a.refresh(ctx)
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
