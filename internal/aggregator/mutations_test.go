package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type mutationCall struct {
	email      string
	payload    xui.ClientPayload
	inboundIDs []int
}

type mutationPanel struct {
	mu sync.Mutex

	clients    []xui.ClientSummary
	details    map[string]xui.ClientDetail
	detailErrs map[string]error
	inbound    xui.InboundDocument
	inboundErr error

	listCalls   int
	detailCalls []string
	addCalls    []mutationCall
	attachCalls []mutationCall
	detachCalls []mutationCall
	addErr      error
	attachErr   error
	detachErrs  map[string]error
	attachBlock chan struct{}
	attachStart chan struct{}
	detachBlock chan struct{}
	detachStart chan struct{}
	validateErr error
}

func (f *mutationPanel) Validate(context.Context) (xui.ServerStatus, error) {
	return xui.ServerStatus{PanelVersion: "3.4.2"}, f.validateErr
}

func (f *mutationPanel) ListClients(context.Context) ([]xui.ClientSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]xui.ClientSummary(nil), f.clients...), nil
}

func (f *mutationPanel) GetClient(_ context.Context, email string) (xui.ClientDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailCalls = append(f.detailCalls, email)
	if err := f.detailErrs[email]; err != nil {
		return xui.ClientDetail{}, err
	}
	return f.details[email], nil
}

func (f *mutationPanel) AddClient(_ context.Context, payload xui.ClientPayload, inboundIDs []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls = append(f.addCalls, mutationCall{payload: payload, email: payload.Email, inboundIDs: slices.Clone(inboundIDs)})
	return f.addErr
}

func (*mutationPanel) UpdateClient(context.Context, string, xui.ClientPayload) error { return nil }
func (*mutationPanel) DeleteClient(context.Context, string) error                    { return nil }

func (f *mutationPanel) AttachClient(ctx context.Context, email string, inboundIDs []int) error {
	f.mu.Lock()
	f.attachCalls = append(f.attachCalls, mutationCall{email: email, inboundIDs: slices.Clone(inboundIDs)})
	block, started, err := f.attachBlock, f.attachStart, f.attachErr
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *mutationPanel) DetachClient(ctx context.Context, email string, inboundIDs []int) error {
	f.mu.Lock()
	f.detachCalls = append(f.detachCalls, mutationCall{email: email, inboundIDs: slices.Clone(inboundIDs)})
	block, started, err := f.detachBlock, f.detachStart, f.detachErrs[email]
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (*mutationPanel) SubLinks(context.Context, string, string) ([]string, error) { return nil, nil }
func (*mutationPanel) ListSlimInbounds(context.Context) ([]xui.InboundSummary, error) {
	return nil, nil
}
func (f *mutationPanel) GetInbound(context.Context, int) (xui.InboundDocument, error) {
	return f.inbound.Clone(), f.inboundErr
}
func (*mutationPanel) AddInbound(context.Context, xui.InboundDocument) (xui.InboundDocument, error) {
	return nil, nil
}
func (*mutationPanel) UpdateInbound(context.Context, int, xui.InboundDocument) (xui.InboundDocument, error) {
	return nil, nil
}
func (*mutationPanel) DeleteInbound(context.Context, int) error { return nil }

func mutationTestAggregator(t *testing.T, panel *mutationPanel) (*Aggregator, *storage.Store, storage.Server, int64) {
	t.Helper()
	store, user, _ := testStore(t)
	server := createServer(t, store, user.ID, "node", "native-token")
	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(storage.Server) (xui.PanelAPI, error) { return panel, nil }
	return a, store, server, user.ID
}

func inboundDocument(t *testing.T, protocol, remark, network, security string) xui.InboundDocument {
	t.Helper()
	var doc xui.InboundDocument
	raw := `{"id":9,"protocol":` + quoteJSON(t, protocol) + `,"remark":` + quoteJSON(t, remark) +
		`,"streamSettings":{"network":` + quoteJSON(t, network) + `,"security":` + quoteJSON(t, security) + `}}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func intPtr(v int) *int { return &v }

func TestAttachGroupHydratesIDsAndUsesLowestNativeID(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{
			{Email: "z@x", SubID: "target"},
			{Email: "a@x", SubID: "target"},
		},
		details: map[string]xui.ClientDetail{
			"z@x": {Client: xui.ClientRecord{RecordID: 2, Email: "z@x"}},
			"a@x": {Client: xui.ClientRecord{RecordID: 7, Email: "a@x"}},
		},
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil {
		t.Fatal(err)
	}
	if result != (MutationResult{Attempted: 1, Succeeded: 1}) {
		t.Fatalf("result=%+v", result)
	}
	if len(panel.attachCalls) != 1 || panel.attachCalls[0].email != "z@x" || !slices.Equal(panel.attachCalls[0].inboundIDs, []int{9}) {
		t.Fatalf("attach calls=%+v", panel.attachCalls)
	}
}

func TestAttachGroupCreatesVLESSClientWhenTargetHasNoRecord(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{{Email: "group-main", SubID: "other"}},
		inbound: inboundDocument(t, "vless", "Main / Reality", "tcp", "reality"),
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "group", 9)
	if err != nil {
		t.Fatal(err)
	}
	if result != (MutationResult{Attempted: 1, Succeeded: 1}) || len(panel.addCalls) != 1 {
		t.Fatalf("result=%+v add=%+v", result, panel.addCalls)
	}
	call := panel.addCalls[0]
	if call.payload.SubID != "group" || call.payload.Email != "group-Main---Reality" || !call.payload.Enable ||
		call.payload.Flow != "xtls-rprx-vision" || !slices.Equal(call.inboundIDs, []int{9}) {
		t.Fatalf("call=%+v", call)
	}
	if ok, _ := regexp.MatchString(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, call.payload.ID); !ok {
		t.Fatalf("not UUID v4: %q", call.payload.ID)
	}
}

func TestAttachGroupUsesUniqueSanitizedEmail(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{
			{Email: "sub--odd-name", SubID: "other"},
			{Email: "sub--odd-name-2", SubID: "other"},
		},
		inbound: inboundDocument(t, "vless", " odd name ", "ws", "tls"),
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	_, err := a.AttachGroup(context.Background(), userID, server.ID, "sub!", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.addCalls) != 1 || panel.addCalls[0].email != "sub--odd-name-3" || panel.addCalls[0].payload.Flow != "" {
		t.Fatalf("add=%+v", panel.addCalls)
	}
}

func TestAttachGroupAlreadyAttachedIsNoop(t *testing.T) {
	panel := &mutationPanel{clients: []xui.ClientSummary{
		{RecordID: intPtr(8), Email: "z@x", SubID: "target", InboundIDs: []int{9}},
		{RecordID: intPtr(2), Email: "a@x", SubID: "target"},
	}}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil || !result.Noop || result.Attempted != 0 || len(panel.attachCalls) != 0 {
		t.Fatalf("result=%+v err=%v calls=%+v", result, err, panel.attachCalls)
	}
}

func TestAttachGroupFallsBackToLexicographicEmailWhenIDsRemainAbsent(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{{Email: "z@x", SubID: "target"}, {Email: "a@x", SubID: "target"}},
		details: map[string]xui.ClientDetail{
			"z@x": {Client: xui.ClientRecord{Email: "z@x"}},
			"a@x": {Client: xui.ClientRecord{Email: "a@x"}},
		},
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	_, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.attachCalls) != 1 || panel.attachCalls[0].email != "a@x" {
		t.Fatalf("attach=%+v", panel.attachCalls)
	}
}

func TestAttachGroupDetailFailureAbortsBeforeMutation(t *testing.T) {
	raw := errors.New("raw token and upstream body")
	panel := &mutationPanel{
		clients:    []xui.ClientSummary{{Email: "a@x", SubID: "target"}},
		detailErrs: map[string]error{"a@x": raw},
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err == nil || strings.Contains(err.Error(), raw.Error()) || result.Attempted != 0 || len(panel.attachCalls)+len(panel.addCalls) != 0 {
		t.Fatalf("result=%+v err=%v attach=%+v add=%+v", result, err, panel.attachCalls, panel.addCalls)
	}
}

func TestAttachGroupRejectsNonVLESSCreation(t *testing.T) {
	panel := &mutationPanel{inbound: inboundDocument(t, "trojan", "main", "tcp", "tls")}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err == nil || result.Attempted != 0 || len(panel.addCalls) != 0 {
		t.Fatalf("result=%+v err=%v add=%+v", result, err, panel.addCalls)
	}
}

func TestAttachGroupMutationErrorIsOnePOSTAndSafe(t *testing.T) {
	raw := errors.New("Bearer top-secret raw upstream body")
	panel := &mutationPanel{
		clients:   []xui.ClientSummary{{RecordID: intPtr(2), Email: "a@x", SubID: "target"}},
		attachErr: raw,
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err == nil || strings.Contains(err.Error(), "top-secret") || len(panel.attachCalls) != 1 || result.Attempted != 1 || result.Succeeded != 0 {
		t.Fatalf("result=%+v err=%v calls=%+v", result, err, panel.attachCalls)
	}
}

func TestAttachGroupKeepsSuccessfulOutcomeWhenRefreshFails(t *testing.T) {
	panel := &mutationPanel{
		clients:     []xui.ClientSummary{{RecordID: intPtr(2), Email: "a@x", SubID: "target"}},
		validateErr: errors.New("refresh transport failed"),
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil || result != (MutationResult{Attempted: 1, Succeeded: 1}) {
		t.Fatalf("mutation outcome hidden by refresh: result=%+v err=%v", result, err)
	}
	if got := a.Snapshot().Servers; len(got) != 1 || got[0].State != ServerUnavailable {
		t.Fatalf("refresh failure not represented in snapshot: %+v", got)
	}
}

func TestAttachGroupMatchesExactSubID(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{{RecordID: intPtr(2), Email: "other", SubID: "target-extra"}},
		inbound: inboundDocument(t, "vless", "main", "tcp", "none"),
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	_, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil || len(panel.attachCalls) != 0 || len(panel.addCalls) != 1 {
		t.Fatalf("err=%v attach=%+v add=%+v", err, panel.attachCalls, panel.addCalls)
	}
}

func TestDetachGroupAttemptsEveryExactAttachedEmail(t *testing.T) {
	raw := errors.New("Bearer raw-token response-body")
	panel := &mutationPanel{
		clients: []xui.ClientSummary{
			{Email: "b@x", SubID: "target", InboundIDs: []int{9}},
			{Email: "a@x", SubID: "target", InboundIDs: []int{9}},
			{Email: "c@x", SubID: "target", InboundIDs: []int{8}},
			{Email: "other@x", SubID: "target-extra", InboundIDs: []int{9}},
		},
		detachErrs: map[string]error{"a@x": raw},
	}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.DetachGroup(context.Background(), userID, server.ID, "target", 9)
	if result != (MutationResult{Attempted: 2, Succeeded: 1}) || err == nil || strings.Contains(err.Error(), "raw-token") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := []string{panel.detachCalls[0].email, panel.detachCalls[1].email}; !slices.Equal(got, []string{"a@x", "b@x"}) {
		t.Fatalf("detach order=%v", got)
	}
}

func TestDetachGroupNoExactAttachmentIsNoop(t *testing.T) {
	panel := &mutationPanel{clients: []xui.ClientSummary{{Email: "a@x", SubID: "target", InboundIDs: []int{8}}}}
	a, _, server, userID := mutationTestAggregator(t, panel)

	result, err := a.DetachGroup(context.Background(), userID, server.ID, "target", 9)
	if err != nil || !result.Noop || len(panel.detachCalls) != 0 {
		t.Fatalf("result=%+v err=%v calls=%+v", result, err, panel.detachCalls)
	}
}

func TestGroupMutationsEnforceOwnership(t *testing.T) {
	panel := &mutationPanel{}
	a, store, server, _ := mutationTestAggregator(t, panel)
	foreign, err := store.CreateUser("foreign", "unused", false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.AttachGroup(context.Background(), foreign.ID, server.ID, "target", 9); err == nil {
		t.Fatal("foreign attach succeeded")
	}
	if _, err := a.DetachGroup(context.Background(), foreign.ID, server.ID, "target", 9); err == nil {
		t.Fatal("foreign detach succeeded")
	}
	if panel.listCalls != 0 {
		t.Fatalf("panel list calls=%d", panel.listCalls)
	}
}

func TestAttachGroupRejectsRotatedGenerationAfterPOST(t *testing.T) {
	panel := &mutationPanel{
		clients:     []xui.ClientSummary{{RecordID: intPtr(2), Email: "a@x", SubID: "target"}},
		attachBlock: make(chan struct{}),
		attachStart: make(chan struct{}, 1),
	}
	a, store, server, userID := mutationTestAggregator(t, panel)
	done := make(chan error, 1)
	go func() {
		_, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
		done <- err
	}()
	<-panel.attachStart
	fresh, err := store.ServerByID(userID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.APIToken = "rotated-token"
	if err := store.UpdateServer(fresh); err != nil {
		t.Fatal(err)
	}
	close(panel.attachBlock)
	if err := <-done; !errors.Is(err, errStaleConnection) {
		t.Fatalf("err=%v", err)
	}
}

func TestAttachGroupRejectsDeletedGenerationAfterPOST(t *testing.T) {
	panel := &mutationPanel{
		clients:     []xui.ClientSummary{{RecordID: intPtr(2), Email: "a@x", SubID: "target"}},
		attachBlock: make(chan struct{}),
		attachStart: make(chan struct{}, 1),
	}
	a, store, server, userID := mutationTestAggregator(t, panel)
	done := make(chan error, 1)
	go func() {
		_, err := a.AttachGroup(context.Background(), userID, server.ID, "target", 9)
		done <- err
	}()
	<-panel.attachStart
	if err := store.DeleteServer(userID, server.ID); err != nil {
		t.Fatal(err)
	}
	close(panel.attachBlock)
	if err := <-done; !errors.Is(err, errStaleConnection) {
		t.Fatalf("err=%v", err)
	}
	if len(a.Snapshot().Servers) != 0 {
		t.Fatalf("deleted server published: %+v", a.Snapshot().Servers)
	}
}

func TestDetachGroupRejectsRotatedGenerationAfterPOST(t *testing.T) {
	panel := &mutationPanel{
		clients:     []xui.ClientSummary{{Email: "a@x", SubID: "target", InboundIDs: []int{9}}},
		detachBlock: make(chan struct{}),
		detachStart: make(chan struct{}, 1),
	}
	a, store, server, userID := mutationTestAggregator(t, panel)
	done := make(chan error, 1)
	go func() {
		_, err := a.DetachGroup(context.Background(), userID, server.ID, "target", 9)
		done <- err
	}()
	<-panel.detachStart
	fresh, err := store.ServerByID(userID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.APIToken = "rotated-token"
	if err := store.UpdateServer(fresh); err != nil {
		t.Fatal(err)
	}
	close(panel.detachBlock)
	if err := <-done; !errors.Is(err, errStaleConnection) {
		t.Fatalf("err=%v", err)
	}
	if len(panel.detachCalls) != 1 {
		t.Fatalf("stale detach retried: %+v", panel.detachCalls)
	}
}

func TestDetachGroupRefusesDeletedServerBeforeInventory(t *testing.T) {
	panel := &mutationPanel{clients: []xui.ClientSummary{{Email: "a@x", SubID: "target", InboundIDs: []int{9}}}}
	a, store, server, userID := mutationTestAggregator(t, panel)
	if err := store.DeleteServer(userID, server.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := a.DetachGroup(context.Background(), userID, server.ID, "target", 9); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if panel.listCalls != 0 || len(panel.detachCalls) != 0 {
		t.Fatalf("deleted server reached panel: lists=%d detach=%+v", panel.listCalls, panel.detachCalls)
	}
}
